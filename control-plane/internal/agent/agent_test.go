package agent

import (
	"context"
	"testing"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestCheckAuth_NoTokenRequired(t *testing.T) {
	s := &AgentServer{agentToken: ""}
	ctx := context.Background()
	err := s.checkAuth(ctx)
	if err != nil {
		t.Errorf("expected no error when token is empty, got: %v", err)
	}
}

func TestCheckAuth_MissingMetadata(t *testing.T) {
	s := &AgentServer{agentToken: "secret-token"}
	ctx := context.Background()
	err := s.checkAuth(ctx)

	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("expected gRPC status error")
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %v", st.Code())
	}
}

func TestCheckAuth_MissingToken(t *testing.T) {
	s := &AgentServer{agentToken: "secret-token"}
	md := metadata.New(map[string]string{}) // empty metadata
	ctx := metadata.NewIncomingContext(context.Background(), md)
	err := s.checkAuth(ctx)

	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("expected gRPC status error")
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated for missing token, got %v", st.Code())
	}
}

func TestCheckAuth_ValidToken(t *testing.T) {
	s := &AgentServer{agentToken: "secret-token"}
	md := metadata.New(map[string]string{
		"authorization": "Bearer secret-token",
	})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	err := s.checkAuth(ctx)
	if err != nil {
		t.Errorf("expected no error for valid token, got: %v", err)
	}
}

func TestCheckAuth_InvalidToken(t *testing.T) {
	s := &AgentServer{agentToken: "secret-token"}
	md := metadata.New(map[string]string{
		"authorization": "Bearer wrong-token",
	})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	err := s.checkAuth(ctx)

	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("expected gRPC status error")
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied for wrong token, got %v", st.Code())
	}
}

func TestCheckAuth_WrongPrefix(t *testing.T) {
	s := &AgentServer{agentToken: "secret-token"}
	md := metadata.New(map[string]string{
		"authorization": "Basic secret-token",
	})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	err := s.checkAuth(ctx)

	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("expected gRPC status error")
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied for wrong prefix, got %v", st.Code())
	}
}

func TestRegister_RejectsNilResources(t *testing.T) {
	reg := registry.NewNodeRegistry()
	s := NewAgentServer(reg, "cluster-test", "localhost:9000", "")

	_, err := s.Register(context.Background(), &computev1.RegisterRequest{
		NodeId:       "node-1",
		AgentVersion: "0.1.0",
	})

	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for nil resources, got %v (err=%v)", got, err)
	}
	if got := len(reg.GetAll()); got != 0 {
		t.Fatalf("expected no registered node after rejected registration, got %d", got)
	}
}

func TestRegister_AcceptsResources(t *testing.T) {
	reg := registry.NewNodeRegistry()
	s := NewAgentServer(reg, "cluster-test", "localhost:9000", "")

	resp, err := s.Register(context.Background(), &computev1.RegisterRequest{
		NodeId:       "node-1",
		AgentVersion: "0.1.0",
		Resources:    &computev1.NodeResources{NodeId: "node-1", Hostname: "worker-1"},
	})
	if err != nil {
		t.Fatalf("expected successful registration, got: %v", err)
	}
	if !resp.Accepted {
		t.Error("expected registration to be accepted")
	}
	if got := len(reg.GetAll()); got != 1 {
		t.Fatalf("expected one registered node, got %d", got)
	}
}

func TestCheckAuth_EmptyTokenValue(t *testing.T) {
	s := &AgentServer{agentToken: "secret-token"}
	md := metadata.New(map[string]string{
		"authorization": "",
	})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	err := s.checkAuth(ctx)

	st, ok := status.FromError(err)
	if !ok {
		t.Fatal("expected gRPC status error")
	}
	if st.Code() != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied for empty token, got %v", st.Code())
	}
}
