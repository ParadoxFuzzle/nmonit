package agent

import (
	"context"
	"strings"
	"time"

	"github.com/ParadoxFuzzle/control-plane/internal/audit"
	"github.com/ParadoxFuzzle/control-plane/internal/identity"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Identity is the resolved caller attached to the request context after a
// successful per-node-token authentication. Retrieve with IdentityFrom.
type Identity struct {
	UserID  string
	Email   string
	Role    identity.Role
	TokenID string
	NodeID  string // empty until Register binds the node
	Via     string // "bootstrap" | "node-token"
}

type identityKey struct{}

// IdentityFrom returns the authenticated identity, or nil when the request
// was authorized via the shared bootstrap token (trusted-LAN mode).
func IdentityFrom(ctx context.Context) *Identity {
	id, _ := ctx.Value(identityKey{}).(*Identity)
	return id
}

// Authenticator resolves agent credentials. It validates, in order:
//  1. the shared bootstrap token (--agent-token) — trusted-LAN / owner path,
//  2. a per-node "nmt_..." token looked up (by sha256) in the identity store.
//
// bootstrap may be empty only when a store is configured; if both are empty,
// auth is disabled entirely (trusted-LAN default, unchanged behavior).
type Authenticator struct {
	bootstrap identity.OwnerBootstrapToken
	store     identity.Store
	audit     *audit.Audit
	limiter   *identity.RateLimiter // per-credential gRPC rate limiting (nil = off)
}

// NewAuthenticator wires an Authenticator. store/audit may be nil.
func NewAuthenticator(bootstrapToken string, store identity.Store, aud *audit.Audit) *Authenticator {
	return &Authenticator{
		bootstrap: identity.OwnerBootstrapToken{Token: bootstrapToken},
		store:     store,
		audit:     aud,
	}
}

// SetRateLimiter enables per-credential rate limiting (e.g. 20 req/s, burst 40).
// Bootstrap-token callers bypass the limiter.
func (a *Authenticator) SetRateLimiter(rate float64, burst int) {
	a.limiter = identity.NewRateLimiter(rate, burst)
}

// bearer extracts the raw bearer credential from gRPC metadata.
func bearer(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}
	tokens := md.Get("authorization")
	if len(tokens) == 0 {
		return "", status.Error(codes.Unauthenticated, "missing authorization token")
	}
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "Bearer ") {
			return strings.TrimPrefix(tok, "Bearer "), nil
		}
	}
	return "", status.Error(codes.Unauthenticated, "missing authorization token")
}

// Authenticate resolves the presented bearer into an Identity, or returns a
// gRPC status error. auditActor is used for audit entries on failures.
func (a *Authenticator) Authenticate(ctx context.Context, auditActor string) (*Identity, error) {
	presented, err := bearer(ctx)
	if err != nil {
		return nil, err
	}

	// 1) Shared bootstrap token (trusted-LAN / owner path) — not rate limited.
	if a.bootstrap.Matches(presented) {
		return &Identity{Via: "bootstrap"}, nil
	}

	// Rate limit everyone else by credential hash.
	if a.limiter != nil && !a.limiter.Allow(presented) {
		a.auditFail(ctx, auditActor, "", "rate limited")
		return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}

	// 2) Per-node token via the identity store.
	if a.store != nil && strings.HasPrefix(presented, "nmt_") {
		hash := identity.HashToken(presented)
		nt, err := a.store.GetTokenByHash(ctx, hash)
		if err == nil {
			if nt.Status != "active" {
				a.auditFail(ctx, auditActor, nt.ID, "token revoked")
				return nil, status.Error(codes.PermissionDenied, "token revoked")
			}
			u, err := a.store.GetUser(ctx, nt.UserID)
			if err != nil {
				log.Error().Err(err).Str("token_id", nt.ID).Msg("auth: token user missing")
				return nil, status.Error(codes.PermissionDenied, "invalid token")
			}
			switch u.Status {
			case identity.StatusActive:
				// ok
			case identity.StatusPending:
				return nil, status.Error(codes.PermissionDenied, "account pending approval")
			case identity.StatusDisabled:
				a.auditFail(ctx, auditActor, nt.ID, "user disabled")
				return nil, status.Error(codes.PermissionDenied, "account disabled")
			}
			_ = a.store.TouchToken(ctx, nt.ID, time.Now())
			return &Identity{
				UserID:  u.ID,
				Email:   u.Email,
				Role:    u.Role,
				TokenID: nt.ID,
				Via:     "node-token",
			}, nil
		}
	}

	a.auditFail(ctx, auditActor, "", "no matching credential")
	return nil, status.Error(codes.PermissionDenied, "invalid token")
}

func (a *Authenticator) auditFail(ctx context.Context, actor, tokenID, reason string) {
	if a.audit == nil {
		return
	}
	a.audit.Append(ctx, audit.Entry{
		Actor: actor, Action: "auth.fail", Object: tokenID,
		Detail: map[string]any{"reason": reason}, Outcome: "denied",
	})
}

// Check validates the presented credential and attaches the resolved
// identity to the returned context. Errors are gRPC status errors.
func (a *Authenticator) Check(ctx context.Context, auditActor string) (context.Context, error) {
	id, err := a.Authenticate(ctx, auditActor)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, identityKey{}, id), nil
}
