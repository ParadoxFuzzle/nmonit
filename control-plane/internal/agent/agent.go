// Package agent implements the gRPC AgentService server side of the control plane.
//
// AgentServer accepts Register/Heartbeat/ExecuteTask/AllocateMemory/etc. from
// compute nodes and writes their state into an injected *registry.NodeRegistry.
//
// Auth and per-field validation are factored out into small helpers
// (checkAuth, AuthUnaryInterceptorFor, validateBearerToken, validateAll) so
// the seven RPC handlers stay focused on what to do once input has been
// validated. Streaming RPCs call checkAuth inline because gRPC stream
// interceptors wrap the stream object, not the open call.
package agent

import (
	"context"
	"crypto/subtle"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/registry"
	"github.com/ParadoxFuzzle/control-plane/internal/validator"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// AgentServer implements the AgentService gRPC server.
type AgentServer struct {
	computev1.UnimplementedAgentServiceServer
	registry   *registry.NodeRegistry
	clusterID  string
	leaderAddr string

	// Shared secret for agent authentication (empty = no auth).
	agentToken string
}

// NewAgentServer constructs an AgentServer wired to the given registry.
func NewAgentServer(reg *registry.NodeRegistry, clusterID, leaderAddr, agentToken string) *AgentServer {
	return &AgentServer{
		registry:   reg,
		clusterID:  clusterID,
		leaderAddr: leaderAddr,
		agentToken: agentToken,
	}
}

// checkAuth applies AgentServer's bearer-token policy to a unary context.
// Streaming RPCs call this inline (see Heartbeat).
func (s *AgentServer) checkAuth(ctx context.Context) error {
	return validateBearerToken(ctx, s.agentToken)
}

// AuthUnaryInterceptorFor returns a gRPC unary interceptor that applies
// AgentServer's bearer-token policy to every unary RPC before dispatch.
// Use this from cmd/control-plane/main.go via grpc.ChainUnaryInterceptor.
func AuthUnaryInterceptorFor(agentToken string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := validateBearerToken(ctx, agentToken); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// validateBearerToken is the shared core of checkAuth / AuthUnaryInterceptorFor.
// Returns nil if expectedToken is empty (no auth configured) or if the
// supplied "authorization" metadata token matches; otherwise a gRPC status
// error so callers can `return nil, err`.
func validateBearerToken(ctx context.Context, expectedToken string) error {
	if expectedToken == "" {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	tokens := md.Get("authorization")
	if len(tokens) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization token")
	}
	expected := "Bearer " + expectedToken
	for _, tok := range tokens {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(expected)) == 1 {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "invalid token")
}

// logValidationRejection emits a WARN-level audit entry on validation failure.
// It pulls the peer address from the gRPC context so rejected attempts are
// traceable. Beyond the field name and a free-form detail, the audit line
// carries a structured source-location record so an operator can jump
// straight to the failing rule:
//   - chain_pos    : zero-based index of the failing check inside its parent chain.
//   - source_file  : repo-relative path of the call site that declared the check.
//   - source_line  : 1-based line number within source_file.
func logValidationRejection(ctx context.Context, method, field, detail, sourceFile string, sourceLine int, position int) {
	remoteIP := "unknown"
	if p, ok := peer.FromContext(ctx); ok {
		remoteIP = p.Addr.String()
	}
	log.Warn().
		Str("remote_ip", remoteIP).
		Str("method", method).
		Str("field", field).
		// chain_pos: zero-based chain index of the failing check.
		Int("chain_pos", position).
		// Structured source-location: lets operators jump from the audit
		// line straight to the file:line that declared the check.
		Str("source_file", sourceFile).
		Int("source_line", sourceLine).
		Str("detail", detail).
		Msg("validation rejected")
}

// validationCheck describes one input-validation rule for a gRPC method.
// `field` is used for the audit-log entry on failure. `sourceFile` and
// `sourceLine` are auto-populated by newCheck at construction time so the
// audit log records exactly where in the codebase each rule was declared.
type validationCheck struct {
	field      string
	sourceFile string
	sourceLine int
	check      func() error
}

// newCheck constructs a validationCheck and records the file:line of its
// call site so audit-log failures can be traced to a precise code location.
//
// The runtime.Caller depth is 1 inside this helper. From newCheck's
// perspective the stack at the moment runtime.Caller runs is:
//   - skip 0: the line in this function where runtime.Caller is invoked.
//   - skip 1: the caller of newCheck — i.e. the AgentServer handler (or test)
//     that wrote `newCheck(...)`. That's the location we want on the audit log.
//
// (Skip is intentionally 1, not 2: Go's testing framework and gRPC infrastructure
// add extra frames above production callers, so a fixed larger skip would land
// on the test runner or gRPC server, not the call site we care about. Skip=1
// always points to the immediate caller of newCheck regardless of context.)
func newCheck(field string, check func() error) validationCheck {
	_, file, line, ok := runtime.Caller(1)
	if !ok {
		file = "unknown"
		line = 0
	} else {
		file = trimToRepoPath(file)
	}
	return validationCheck{
		field:      field,
		sourceFile: file,
		sourceLine: line,
		check:      check,
	}
}

// trimToRepoPath converts an absolute file path to a repo-relative one so
// audit logs are IDE-navigable rather than host-machinery-dependent.
//
// Heuristic, in priority order:
//  1. The first "/internal/" segment (this project keeps all production Go
//     code under internal/). Trim to "internal/..." — the module-name prefix
//     is dropped on purpose; if a future maintainer wants module disambiguation
//     they can wire module markers as the FIRST preference instead.
//  2. First occurrence of "/control-plane/" or "/cli/" — keeps the module
//     name in the path so cross-module audit consumers can tell them apart.
//  3. filepath.Base as a last-ditch fallback for unrelated paths.
//
// Note: First-occurrence (`strings.Index`) is used in priority 2 because the
// module root appears exactly once at the start of the path; a project path
// like `/repo/control-plane/cmd/control-plane/main.go` contains the substring
// twice, and only the FIRST is the project's module root.
func trimToRepoPath(abs string) string {
	if i := strings.Index(abs, "/internal/"); i >= 0 {
		return abs[i+1:] // drop leading "/"; keep "internal/..."
	}
	for _, marker := range []string{"control-plane/", "cli/"} {
		if i := strings.Index(abs, "/"+marker); i >= 0 {
			return abs[i+1:]
		}
	}
	return filepath.Base(abs)
}

// validateAll runs each check in order and returns the first failure.
// On failure it emits the audit log via logValidationRejection (carrying
// the failing check's zero-based chain index and the auto-captured
// source_file/source_line) and returns the gRPC status error so callers can
// simply `return nil, err`.
func validateAll(ctx context.Context, method string, checks []validationCheck) error {
	for i, c := range checks {
		if err := c.check(); err != nil {
			logValidationRejection(ctx, method, c.field, err.Error(), c.sourceFile, c.sourceLine, i)
			return err
		}
	}
	return nil
}

// ============================================================================
// RPC handlers
// ============================================================================

func (s *AgentServer) Register(ctx context.Context, req *computev1.RegisterRequest) (*computev1.RegisterResponse, error) {
	if err := validateAll(ctx, "Register", []validationCheck{
		newCheck("node_id", func() error { return validator.ValidateNodeID(req.NodeId) }),
		newCheck("address", func() error { return validator.ValidateAddress(req.Address) }),
		newCheck("agent_version", func() error { return validator.ValidateVersion(req.AgentVersion) }),
	}); err != nil {
		return nil, err
	}

	log.Info().
		Str("node_id", req.NodeId).
		Str("address", req.Address).
		Str("version", req.AgentVersion).
		Msg("node registering")

	s.registry.Register(req.NodeId, req.Address, req.Resources)

	return &computev1.RegisterResponse{
		Accepted:            true,
		ClusterId:           s.clusterID,
		ControlPlaneLeader:  s.leaderAddr,
		HeartbeatIntervalMs: 5000,
	}, nil
}

func (s *AgentServer) Heartbeat(stream computev1.AgentService_HeartbeatServer) error {
	// Auth check — reject unauthenticated streams before processing.
	// Stream RPCs can't use the unary interceptor (it wraps a single call,
	// not an open-ended stream), so checkAuth is called inline here.
	if err := s.checkAuth(stream.Context()); err != nil {
		return err
	}

	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	nodeID := first.NodeId

	if err := validateAll(stream.Context(), "Heartbeat", []validationCheck{
		newCheck("node_id", func() error { return validator.ValidateNodeID(nodeID) }),
		newCheck("tasks", func() error { return validator.ValidateHeartbeatTaskCount(len(first.Tasks)) }),
	}); err != nil {
		return err
	}

	connID := s.registry.ConnectionID(nodeID)

	log.Info().Str("node_id", nodeID).Uint64("conn_id", connID).Msg("heartbeat stream opened")

	s.registry.UpdateHeartbeat(first.NodeId, first.Utilization, first.Tasks)

	done := make(chan struct{})
	defer close(done)

	if err := stream.Send(&computev1.HeartbeatResponse{
		Acknowledged: true,
		Sequence:     first.Sequence,
	}); err != nil {
		log.Warn().Err(err).Msg("failed to send initial heartbeat ack")
		return err
	}

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := stream.Send(&computev1.HeartbeatResponse{
					Acknowledged: true,
				}); err != nil {
					log.Warn().Err(err).Msg("failed to send heartbeat ack")
					return
				}
			}
		}
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			log.Info().Str("node_id", nodeID).Msg("heartbeat stream closed")
			s.registry.RemoveIfConnectionMatches(nodeID, connID)
			return nil
		}
		if err != nil {
			log.Warn().Err(err).Str("node_id", nodeID).Msg("heartbeat stream error")
			s.registry.RemoveIfConnectionMatches(nodeID, connID)
			return err
		}

		if err := validateAll(stream.Context(), "Heartbeat", []validationCheck{
			newCheck("node_id", func() error { return validator.ValidateNodeID(req.NodeId) }),
			newCheck("tasks", func() error { return validator.ValidateHeartbeatTaskCount(len(req.Tasks)) }),
		}); err != nil {
			return err
		}

		s.registry.UpdateHeartbeat(req.NodeId, req.Utilization, req.Tasks)

		log.Debug().
			Str("node_id", req.NodeId).
			Uint64("sequence", req.Sequence).
			Msg("heartbeat received")
	}
}

func (s *AgentServer) ExecuteTask(ctx context.Context, req *computev1.TaskSpec) (*computev1.TaskAck, error) {
	if err := validateAll(ctx, "ExecuteTask", []validationCheck{
		newCheck("task_id", func() error { return validator.ValidateTaskID(req.TaskId) }),
		newCheck("job_id", func() error { return validator.ValidateJobID(req.JobId) }),
		newCheck("container_image", func() error { return validator.ValidateContainerImage(req.ContainerImage) }),
		newCheck("command", func() error { return validator.ValidateCommands(req.Command) }),
		newCheck("env", func() error { return validator.ValidateEnvVars(req.Env) }),
		newCheck("priority", func() error { return validator.ValidatePriority(req.Priority) }),
	}); err != nil {
		return nil, err
	}
	log.Info().
		Str("task_id", req.TaskId).
		Str("image", req.ContainerImage).
		Msg("task execution requested")

	// TODO: Implement actual task dispatching to the target agent.
	// For now, accept the task and log the request.
	return &computev1.TaskAck{
		Accepted: true,
	}, nil
}

func (s *AgentServer) AllocateMemory(ctx context.Context, req *computev1.MemoryAllocRequest) (*computev1.MemoryAllocResponse, error) {
	if err := validateAll(ctx, "AllocateMemory", []validationCheck{
		newCheck("allocation_id", func() error { return validator.ValidateAllocationID(req.AllocationId) }),
		newCheck("size_bytes", func() error { return validator.ValidateMemorySize(req.SizeBytes) }),
		newCheck("replication_factor", func() error { return validator.ValidateReplicationFactor(req.ReplicationFactor) }),
	}); err != nil {
		return nil, err
	}
	log.Info().
		Str("allocation_id", req.AllocationId).
		Uint64("size_bytes", req.SizeBytes).
		Msg("memory allocation requested")
	return &computev1.MemoryAllocResponse{
		Success: true,
		Handle: &computev1.MemoryHandle{
			AllocationId: req.AllocationId,
			SizeBytes:    req.SizeBytes,
		},
	}, nil
}

func (s *AgentServer) AllocateGPUMemory(ctx context.Context, req *computev1.GPUMemoryAllocRequest) (*computev1.GPUMemoryAllocResponse, error) {
	if err := validateAll(ctx, "AllocateGPUMemory", []validationCheck{
		newCheck("allocation_id", func() error { return validator.ValidateAllocationID(req.AllocationId) }),
		newCheck("size_bytes", func() error { return validator.ValidateGPUMemorySize(req.SizeBytes) }),
		newCheck("gpu_device_index", func() error { return validator.ValidateGPUDeviceIndex(req.GpuDeviceIndex) }),
	}); err != nil {
		return nil, err
	}
	log.Info().
		Str("allocation_id", req.AllocationId).
		Uint64("size_bytes", req.SizeBytes).
		Msg("GPU memory allocation requested")
	return &computev1.GPUMemoryAllocResponse{
		Success: true,
		Handle: &computev1.GPUHandle{
			AllocationId: req.AllocationId,
			SizeBytes:    req.SizeBytes,
		},
	}, nil
}

func (s *AgentServer) FreeMemory(ctx context.Context, req *computev1.FreeMemoryRequest) (*computev1.FreeMemoryResponse, error) {
	if err := validateAll(ctx, "FreeMemory", []validationCheck{
		newCheck("allocation_id", func() error { return validator.ValidateAllocationID(req.AllocationId) }),
	}); err != nil {
		return nil, err
	}
	log.Info().Str("allocation_id", req.AllocationId).Msg("memory free requested")
	return &computev1.FreeMemoryResponse{Success: true}, nil
}

func (s *AgentServer) FreeGPUMemory(ctx context.Context, req *computev1.FreeGPUMemoryRequest) (*computev1.FreeGPUMemoryResponse, error) {
	if err := validateAll(ctx, "FreeGPUMemory", []validationCheck{
		newCheck("allocation_id", func() error { return validator.ValidateAllocationID(req.AllocationId) }),
	}); err != nil {
		return nil, err
	}
	log.Info().Str("allocation_id", req.AllocationId).Msg("GPU memory free requested")
	return &computev1.FreeGPUMemoryResponse{Success: true}, nil
}
