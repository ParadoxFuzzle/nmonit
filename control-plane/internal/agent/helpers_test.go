package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// captureLogger swaps the package-level zerolog logger for one writing to the
// returned buffer, restoring the original logger via t.Cleanup. Tests that
// inspect audit-log output use this; tests that don't inspect logs can run
// without it (the global singleton goes through the default stderr writer).
func captureLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = orig })
	return &buf
}

// ============================================================================
// validateAll
// ============================================================================

func TestValidateAll_EmptyChecksReturnsNil(t *testing.T) {
	if err := validateAll(context.Background(), "EmptyMethod", nil); err != nil {
		t.Errorf("expected nil for empty checks, got: %v", err)
	}
}

func TestValidateAll_AllChecksPassInOrder(t *testing.T) {
	var calls []string
	checks := []validationCheck{
		newCheck("a", func() error { calls = append(calls, "a"); return nil }),
		newCheck("b", func() error { calls = append(calls, "b"); return nil }),
		newCheck("c", func() error { calls = append(calls, "c"); return nil }),
	}

	if err := validateAll(context.Background(), "AllPassMethod", checks); err != nil {
		t.Fatalf("expected nil when every check passes, got: %v", err)
	}
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("expected all checks invoked in order, got %v", calls)
	}
}

func TestValidateAll_ShortCircuitsOnFirstFailure(t *testing.T) {
	buf := captureLogger(t)

	boom := errors.New("boom")
	downstreamCalls := 0

	checks := []validationCheck{
		newCheck("first", func() error { return boom }),
		newCheck("second", func() error {
			downstreamCalls++
			return nil
		}),
		newCheck("third", func() error {
			downstreamCalls++
			return nil
		}),
	}

	if err := validateAll(context.Background(), "ShortCircuitMethod", checks); !errors.Is(err, boom) {
		t.Errorf("expected to bubble up 'boom', got: %v", err)
	}
	if downstreamCalls != 0 {
		t.Errorf("checks after the failing one should not run, but %d were invoked", downstreamCalls)
	}

	// Failing check is the FIRST (index 0), so chain_pos must be 0.
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("audit log not valid JSON: %v\nbody: %s", err, buf.String())
	}
	if got, _ := entry["chain_pos"].(float64); got != 0 {
		t.Errorf("expected chain_pos=0 (first check failed), got %v", entry["chain_pos"])
	}
}

func TestValidateAll_ReturnsFirstFailureOnly(t *testing.T) {
	buf := captureLogger(t)

	firstErr := errors.New("first error")
	secondErr := errors.New("second error")
	var calls []string

	checks := []validationCheck{
		newCheck("x", func() error { calls = append(calls, "x"); return nil }),
		newCheck("y", func() error { calls = append(calls, "y"); return firstErr }),
		newCheck("z", func() error { calls = append(calls, "z"); return secondErr }),
	}

	if err := validateAll(context.Background(), "FirstErrorMethod", checks); !errors.Is(err, firstErr) {
		t.Errorf("expected firstErr, got: %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"x", "y"}) {
		t.Errorf("expected only first two checks invoked, got %v", calls)
	}

	// Failing check is at index 1 ("y") — not 0 ("x") and not 2 ("z").
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("audit log not valid JSON: %v\nbody: %s", err, buf.String())
	}
	if got, _ := entry["chain_pos"].(float64); got != 1 {
		t.Errorf("expected chain_pos=1 (middle check failed), got %v", entry["chain_pos"])
	}
}

func TestValidateAll_LogsAuditEntryOnFailure(t *testing.T) {
	buf := captureLogger(t)

	failing := errors.New("invalid value")
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 4242},
	})

	if err := validateAll(ctx, "AuditMethod", []validationCheck{
		newCheck("node_id", func() error { return failing }),
	}); !errors.Is(err, failing) {
		t.Fatalf("expected failing error, got: %v", err)
	}

	if buf.Len() == 0 {
		t.Fatalf("expected audit log on failure, got empty buffer")
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("audit log not valid JSON: %v\nbody: %s", err, buf.String())
	}
	if msg, _ := entry["message"].(string); msg != "validation rejected" {
		t.Errorf("expected message 'validation rejected', got %q", msg)
	}
	if entry["method"] != "AuditMethod" {
		t.Errorf("expected method=AuditMethod, got %v", entry["method"])
	}
	if entry["field"] != "node_id" {
		t.Errorf("expected field=node_id, got %v", entry["field"])
	}
	if entry["detail"] != "invalid value" {
		t.Errorf("expected detail='invalid value', got %v", entry["detail"])
	}
	if entry["remote_ip"] != "10.0.0.1:4242" {
		t.Errorf("expected remote_ip=10.0.0.1:4242, got %v", entry["remote_ip"])
	}
	// Position is the failing check's zero-based index in the chain.
	// JSON parses numbers as float64.
	if got, _ := entry["chain_pos"].(float64); got != 0 {
		t.Errorf("expected chain_pos=0 (zero-based index of failing check), got %v", entry["chain_pos"])
	}
	// Structured source-location: must point at THIS test file at a
	// positive 1-based line. We don't pin the exact line (inserting a
	// blank above the newCheck call would shift it), but the file component
	// + a non-zero line number is enough to guarantee runtime.Caller fired.
	if src, _ := entry["source_file"].(string); !strings.Contains(src, "helpers_test.go") {
		t.Errorf("expected source_file to contain 'helpers_test.go', got %q", src)
	}
	if ln, _ := entry["source_line"].(float64); ln <= 0 {
		t.Errorf("expected source_line > 0 (1-based line captured by runtime.Caller), got %v", entry["source_line"])
	}
	// zerolog emits a JSON line per log record; with one Warn call there
	// should be exactly one trailing newline-delimited entry.
	if got := bytes.Count(buf.Bytes(), []byte{'\n'}); got != 1 {
		t.Errorf("expected exactly one log entry, got %d", got)
	}
}

func TestValidateAll_LogsFailingCheckChainPosAtTail(t *testing.T) {
	// Verifies the `chain_pos` field isn't always 0: a check that fails
	// after two passing checks should report chain_pos=2 so operators can
	// identify which rule in the chain tripped.
	buf := captureLogger(t)

	failing := errors.New("tail fail")

	if err := validateAll(context.Background(), "TailMethod", []validationCheck{
		newCheck("a", func() error { return nil }),
		newCheck("b", func() error { return nil }),
		newCheck("c_tail", func() error { return failing }),
	}); !errors.Is(err, failing) {
		t.Fatalf("expected failing error, got: %v", err)
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("audit log not valid JSON: %v\nbody: %s", err, buf.String())
	}
	if entry["field"] != "c_tail" {
		t.Errorf("expected field=c_tail, got %v", entry["field"])
	}
	if got, _ := entry["chain_pos"].(float64); got != 2 {
		t.Errorf("expected chain_pos=2 (third check in a 3-check chain), got %v", entry["chain_pos"])
	}
}

func TestValidateAll_NoLogOnSuccess(t *testing.T) {
	buf := captureLogger(t)

	if err := validateAll(context.Background(), "NoLogMethod", []validationCheck{
		newCheck("a", func() error { return nil }),
		newCheck("b", func() error { return nil }),
	}); err != nil {
		t.Fatalf("expected nil, got: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no audit log on success, got: %s", buf.String())
	}
}

// ============================================================================
// AuthUnaryInterceptorFor
// ============================================================================

func TestAuthUnaryInterceptorFor_NoTokenConfigured_CallsHandler(t *testing.T) {
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "ok", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/Method"}

	resp, err := AuthUnaryInterceptorFor("")(context.Background(), "input", info, handler)
	if err != nil {
		t.Fatalf("expected nil error when token is empty, got: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected response 'ok', got %v", resp)
	}
	if !called {
		t.Error("expected handler to be invoked when token is empty (auth disabled)")
	}
}

func TestAuthUnaryInterceptorFor_MissingMetadata_RejectsBeforeHandler(t *testing.T) {
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return nil, nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/Method"}

	_, err := AuthUnaryInterceptorFor("secret-token")(context.Background(), "input", info, handler)

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got: %v", err)
	}
	if called {
		t.Error("handler must not be invoked when metadata is missing")
	}
}

func TestAuthUnaryInterceptorFor_MissingAuthorizationHeader_RejectsBeforeHandler(t *testing.T) {
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return nil, nil
	}
	// Empty metadata: key set exists but "authorization" is absent.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/Method"}

	_, err := AuthUnaryInterceptorFor("secret-token")(ctx, "input", info, handler)

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated for absent header, got: %v", err)
	}
	if called {
		t.Error("handler must not be invoked when 'authorization' header is absent")
	}
}

func TestAuthUnaryInterceptorFor_WrongToken_RejectsBeforeHandler(t *testing.T) {
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return nil, nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer wrong"))
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/Method"}

	_, err := AuthUnaryInterceptorFor("secret-token")(ctx, "input", info, handler)

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied for wrong token, got: %v", err)
	}
	if called {
		t.Error("handler must not be invoked when token is wrong")
	}
}

func TestAuthUnaryInterceptorFor_WrongScheme_RejectsBeforeHandler(t *testing.T) {
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return nil, nil
	}
	// Token value is correct, but the scheme differs.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Basic secret-token"))
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/Method"}

	_, err := AuthUnaryInterceptorFor("secret-token")(ctx, "input", info, handler)

	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied for non-Bearer scheme, got: %v", err)
	}
	if called {
		t.Error("handler must not be invoked when scheme isn't Bearer")
	}
}

func TestAuthUnaryInterceptorFor_ValidToken_CallsHandler(t *testing.T) {
	called := false
	handler := func(ctx context.Context, req any) (any, error) {
		called = true
		return "result", nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer secret-token"))
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/Method"}

	resp, err := AuthUnaryInterceptorFor("secret-token")(ctx, "input", info, handler)
	if err != nil {
		t.Fatalf("expected nil error for valid token, got: %v", err)
	}
	if !called {
		t.Error("expected handler to be invoked for matching token")
	}
	if resp != "result" {
		t.Errorf("expected handler response 'result' to pass through, got %v", resp)
	}
}

func TestAuthUnaryInterceptorFor_ValidToken_PropagatesHandlerErrorAndResponse(t *testing.T) {
	handlerErr := status.Error(codes.Internal, "downstream boom")
	handler := func(ctx context.Context, req any) (any, error) {
		return "partial-result", handlerErr
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer secret-token"))
	info := &grpc.UnaryServerInfo{FullMethod: "/svc/Method"}

	resp, err := AuthUnaryInterceptorFor("secret-token")(ctx, "input", info, handler)

	want, _ := status.FromError(handlerErr)
	got, _ := status.FromError(err)
	if got.Code() != want.Code() || got.Message() != want.Message() {
		t.Errorf("expected handler error %v to pass through unchanged, got %v", want, got)
	}
	if resp != "partial-result" {
		t.Errorf("expected handler response to pass through unchanged, got %v", resp)
	}
}

// ============================================================================
// validateBearerToken
// ============================================================================
//
// These tests exercise the shared core that backs both AgentServer.checkAuth and
// AuthUnaryInterceptorFor. They pin the exact gRPC code + status message so a
// future edit that changes either surfaces as a failing assertion rather than
// being silently absorbed by a wrapper test.

// bearerMD is a tiny helper that builds an incoming metadata context with the
// given "authorization" value (use empty string for "header absent").
func bearerMD(authValue string) context.Context {
	md := metadata.MD{}
	if authValue != "" {
		md.Set("authorization", authValue)
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

// assertCode extracts the gRPC status code from err, fails the test when err
// is not a status error, and returns the code for further assertions.
func assertCode(t *testing.T, err error) codes.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	return st.Code()
}

func TestValidateBearerToken_NoTokenConfigured_BypassesAuth(t *testing.T) {
	// Empty expectedToken must short-circuit and return nil regardless of
	// metadata state — mirrors the "auth disabled" mode.
	if err := validateBearerToken(context.Background(), ""); err != nil {
		t.Errorf("expected nil error when expectedToken is empty, got: %v", err)
	}
	if err := validateBearerToken(bearerMD("Bearer anything"), ""); err != nil {
		t.Errorf("expected nil error when expectedToken is empty (metadata present), got: %v", err)
	}
}

func TestValidateBearerToken_MissingMetadata_ReturnsUnauthenticated(t *testing.T) {
	// No incoming metadata set on the ctx at all — different from "empty MD".
	if got := assertCode(t, validateBearerToken(context.Background(), "secret-token")); got != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated for missing metadata, got %v", got)
	}
}

func TestValidateBearerToken_MissingAuthorizationHeader_ReturnsUnauthenticated(t *testing.T) {
	// Incoming metadata exists but has no "authorization" key.
	if got := assertCode(t, validateBearerToken(bearerMD(""), "secret-token")); got != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated for absent 'authorization' header, got %v", got)
	}
}

func TestValidateBearerToken_WrongPrefix_ReturnsPermissionDenied(t *testing.T) {
	// Token value is correct, but the scheme isn't "Bearer " — the constant-
	// time comparison must fail and surface PermissionDenied.
	if got := assertCode(t, validateBearerToken(bearerMD("Basic secret-token"), "secret-token")); got != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied for non-Bearer scheme, got %v", got)
	}
}

func TestValidateBearerToken_ValidToken_ReturnsNil(t *testing.T) {
	if err := validateBearerToken(bearerMD("Bearer secret-token"), "secret-token"); err != nil {
		t.Errorf("expected nil for valid Bearer token, got: %v", err)
	}
}

// ============================================================================
// newCheck (runtime.Caller capture of source location)
// ============================================================================

func TestNewCheck_PopulatesSourceLocation(t *testing.T) {
	// Anchor: the line where this runtime.Caller(0) sits. Whatever line
	// newCheck lands on below must be strictly greater, because the
	// newCheck call is below the anchor in source order. This is more
	// robust than a brittle exact-line pin (inserting a blank above the
	// newCheck call wouldn't change the ordering invariant).
	_, _, callerLine, _ := runtime.Caller(0)

	got := newCheck("captured_field", func() error { return nil })

	if got.field != "captured_field" {
		t.Errorf("expected field=captured_field, got %q", got.field)
	}
	// source_file must be repo-relative (trimToRepoPath applied) and
	// must contain the basename of this test file plus the package path.
	if !strings.Contains(got.sourceFile, "helpers_test.go") {
		t.Errorf("expected sourceFile to contain 'helpers_test.go', got %q", got.sourceFile)
	}
	if !strings.Contains(got.sourceFile, "internal/agent/") {
		t.Errorf("expected sourceFile to live under 'internal/agent/', got %q", got.sourceFile)
	}
	// runtime.Caller(2) inside newCheck returns the line of the newCheck
	// call site, which is strictly below the anchor captured above.
	if got.sourceLine <= callerLine {
		t.Errorf("expected sourceLine > callerLine=%d (newCheck call site below the anchor), got %d", callerLine, got.sourceLine)
	}
	if got.check == nil {
		t.Error("expected check func to be preserved")
	}
}

func TestTrimToRepoPath(t *testing.T) {
	// Pin each (input, expected) pair. The /internal/ heuristic strips to
	// "internal/..."; the module-marker fallback strips to e.g.
	// "control-plane/..."; the basename fallback kicks in when nothing matches.
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "control-plane internal",
			input:    "/home/user/nmonit/control-plane/internal/agent/agent.go",
			expected: "internal/agent/agent.go",
		},
		{
			name:     "control-plane cmd",
			input:    "/home/user/nmonit/control-plane/cmd/control-plane/main.go",
			expected: "control-plane/cmd/control-plane/main.go",
		},
		{
			name:     "cli internal",
			input:    "/home/user/nmonit/cli/internal/commands/deploy.go",
			expected: "internal/commands/deploy.go",
		},
		{
			name:     "unrelated path falls back to basename",
			input:    "/tmp/random/foo.go",
			expected: "foo.go",
		},
		{
			name:     "single-segment path falls back to basename",
			input:    "foo.rs",
			expected: "foo.rs",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trimToRepoPath(c.input); got != c.expected {
				t.Errorf("trimToRepoPath(%q) = %q, want %q", c.input, got, c.expected)
			}
		})
	}
}
