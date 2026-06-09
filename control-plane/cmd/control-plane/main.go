package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/compute-nmonit/control-plane/gen/compute/v1"
	"github.com/compute-nmonit/control-plane/internal/metrics"
	"github.com/compute-nmonit/control-plane/internal/tlsreload"
	"github.com/compute-nmonit/control-plane/internal/validator"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ============================================================================
// Node Registry
// ============================================================================

// NodeRegistry holds state about connected agent nodes.
type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeInfo
}

type NodeInfo struct {
	NodeID       string
	Address      string
	Resources    *computev1.NodeResources
	LastBeat     time.Time
	Tasks        map[string]*computev1.TaskStatus
	ConnectionID uint64 // Monotonic counter; heartbeat stream validates against this
}

func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{
		nodes: make(map[string]*NodeInfo),
	}
}

func (r *NodeRegistry) Register(nodeID string, addr string, res *computev1.NodeResources) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Increment connection ID on re-registration so old heartbeat streams
	// don't accidentally remove a reconnected node.
	connID := uint64(1)
	if existing, ok := r.nodes[nodeID]; ok {
		connID = existing.ConnectionID + 1
	}
	r.nodes[nodeID] = &NodeInfo{
		NodeID:       nodeID,
		Address:      addr,
		Resources:    res,
		LastBeat:     time.Now(),
		Tasks:        make(map[string]*computev1.TaskStatus),
		ConnectionID: connID,
	}
	metrics.RegistrationsTotal.Inc()
	metrics.RegisteredNodes.Set(float64(len(r.nodes)))
}

func (r *NodeRegistry) UpdateHeartbeat(nodeID string, util *computev1.NodeUtilization, tasks []*computev1.TaskStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok {
		n.LastBeat = time.Now()
		// Replace the entire task map with current state from the agent.
		// This prevents unbounded memory growth from accumulating completed tasks.
		n.Tasks = make(map[string]*computev1.TaskStatus, len(tasks))
		for _, t := range tasks {
			n.Tasks[t.TaskId] = t
		}
	}
	metrics.HeartbeatsTotal.Inc()
	metrics.TasksRunning.Set(float64(r.totalTasksLocked()))
}

// GetAll returns a deep copy of the node map to prevent data races
// between heartbeat updates and REST API serialization.
func (r *NodeRegistry) GetAll() []NodeResponse {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NodeResponse, 0, len(r.nodes))
	for _, v := range r.nodes {
		out = append(out, NodeResponse{
			NodeID:   v.NodeID,
			Hostname: safeHostname(v.Resources),
			Address:  v.Address,
			CPU:      cpuSummary(v.Resources),
			Memory:   memorySummary(v.Resources),
			GPUs:     gpuSummary(v.Resources),
			LastBeat: v.LastBeat.Format(time.RFC3339),
		})
	}
	return out
}

func (r *NodeRegistry) Remove(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nodes, nodeID)
	metrics.RegisteredNodes.Set(float64(len(r.nodes)))
	metrics.TasksRunning.Set(float64(r.totalTasksLocked()))
}

// RemoveIfConnectionMatches only deletes the node if its connection ID
// still matches. This prevents old heartbeat streams from removing a
// node that has already reconnected under a newer stream.
func (r *NodeRegistry) RemoveIfConnectionMatches(nodeID string, connID uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok && n.ConnectionID == connID {
		delete(r.nodes, nodeID)
		metrics.RegisteredNodes.Set(float64(len(r.nodes)))
		metrics.TasksRunning.Set(float64(r.totalTasksLocked()))
	}
}

// ConnectionID returns the current connection ID for a node, or 0 if not found.
func (r *NodeRegistry) ConnectionID(nodeID string) uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n, ok := r.nodes[nodeID]; ok {
		return n.ConnectionID
	}
	return 0
}

// totalTasksLocked returns the sum of tasks across all registered nodes.
// Caller must hold r.mu (read or write lock).
func (r *NodeRegistry) totalTasksLocked() int {
	total := 0
	for _, n := range r.nodes {
		total += len(n.Tasks)
	}
	return total
}

// StaleNodeCleanup periodically removes nodes that haven't sent a heartbeat
// within the threshold.
func (r *NodeRegistry) StaleNodeCleanup(ctx context.Context, threshold time.Duration) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			now := time.Now()
			removed := 0
			for id, info := range r.nodes {
				if now.Sub(info.LastBeat) > threshold {
					delete(r.nodes, id)
					removed++
					log.Warn().Str("node_id", id).Dur("since_last_beat", now.Sub(info.LastBeat)).Msg("node marked offline (stale)")
				}
			}
			if removed > 0 {
				metrics.StaleNodesRemoved.Add(float64(removed))
				metrics.RegisteredNodes.Set(float64(len(r.nodes)))
				metrics.TasksRunning.Set(float64(r.totalTasksLocked()))
			}
			r.mu.Unlock()
		}
	}
}

// ============================================================================
// REST API Response Types
// ============================================================================

type NodeResponse struct {
	NodeID   string `json:"node_id"`
	Hostname string `json:"hostname"`
	Address  string `json:"address"`
	CPU      CPUSummary   `json:"cpu"`
	Memory   MemorySummary `json:"memory"`
	GPUs     []GPUSummary  `json:"gpus"`
	LastBeat string `json:"last_beat"`
}

type CPUSummary struct {
	PhysicalCores int    `json:"physical_cores"`
	LogicalCores  int    `json:"logical_cores"`
	Architecture  string `json:"architecture"`
	Model         string `json:"model"`
}

type MemorySummary struct {
	TotalGB float64 `json:"total_gb"`
}

type GPUSummary struct {
	Index     int    `json:"index"`
	Vendor    string `json:"vendor"`
	Model     string `json:"model"`
	VRamGB    float64 `json:"vram_gb"`
}

func safeHostname(r *computev1.NodeResources) string {
	if r == nil { return "" }
	return r.Hostname
}

func cpuSummary(r *computev1.NodeResources) CPUSummary {
	if r == nil || r.Cpu == nil {
		return CPUSummary{}
	}
	return CPUSummary{
		PhysicalCores: int(r.Cpu.PhysicalCores),
		LogicalCores:  int(r.Cpu.LogicalCores),
		Architecture:  r.Cpu.Architecture,
		Model:         r.Cpu.ModelName,
	}
}

func memorySummary(r *computev1.NodeResources) MemorySummary {
	if r == nil || r.Memory == nil {
		return MemorySummary{}
	}
	return MemorySummary{
		TotalGB: float64(r.Memory.TotalBytes) / (1024 * 1024 * 1024),
	}
}

func gpuSummary(r *computev1.NodeResources) []GPUSummary {
	if r == nil || len(r.Gpus) == 0 {
		return []GPUSummary{}
	}
	out := make([]GPUSummary, len(r.Gpus))
	for i, g := range r.Gpus {
		out[i] = GPUSummary{
			Index:  int(g.Index),
			Vendor: g.Vendor,
			Model:  g.Model,
			VRamGB: float64(g.VramBytes) / (1024 * 1024 * 1024),
		}
	}
	return out
}

// ============================================================================
// gRPC Agent Server
// ============================================================================

// AgentServer implements the AgentService gRPC server.
type AgentServer struct {
	computev1.UnimplementedAgentServiceServer
	registry  *NodeRegistry
	clusterID string
	leaderAddr string

	// Shared secret for agent authentication (empty = no auth).
	agentToken string
}

func (s *AgentServer) checkAuth(ctx context.Context) error {
	if s.agentToken == "" {
		return nil
	}
	// Extract bearer token from gRPC metadata.
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	tokens := md.Get("authorization")
	if len(tokens) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization token")
	}
	expected := "Bearer " + s.agentToken
	for _, tok := range tokens {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(expected)) == 1 {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "invalid token")
}

// logValidationRejection logs a WARN-level audit entry when an RPC request
// fails input validation. It extracts the remote peer address from the gRPC
// context so rejected attempts are traceable.
func logValidationRejection(ctx context.Context, method, field, detail string) {
	remoteIP := "unknown"
	if p, ok := peer.FromContext(ctx); ok {
		remoteIP = p.Addr.String()
	}
	log.Warn().
		Str("remote_ip", remoteIP).
		Str("method", method).
		Str("field", field).
		Str("detail", detail).
		Msg("validation rejected")
}

func (s *AgentServer) Register(ctx context.Context, req *computev1.RegisterRequest) (*computev1.RegisterResponse, error) {
	if err := s.checkAuth(ctx); err != nil {
		return nil, err
	}
	if err := validator.ValidateNodeID(req.NodeId); err != nil {
		logValidationRejection(ctx, "Register", "node_id", err.Error())
		return nil, err
	}
	if err := validator.ValidateAddress(req.Address); err != nil {
		logValidationRejection(ctx, "Register", "address", err.Error())
		return nil, err
	}
	if err := validator.ValidateVersion(req.AgentVersion); err != nil {
		logValidationRejection(ctx, "Register", "agent_version", err.Error())
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
	// Auth check — reject unauthenticated streams before processing
	if err := s.checkAuth(stream.Context()); err != nil {
		return err
	}

	// Receive first heartbeat to identify the node
	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	nodeID := first.NodeId

	// Validate the initial heartbeat
	if err := validator.ValidateNodeID(nodeID); err != nil {
		logValidationRejection(stream.Context(), "Heartbeat", "node_id", err.Error())
		return err
	}
	if err := validator.ValidateHeartbeatTaskCount(len(first.Tasks)); err != nil {
		logValidationRejection(stream.Context(), "Heartbeat", "tasks", err.Error())
		return err
	}

	// Capture the connection ID at stream open so we only tear down
	// this specific connection, not a newer one from a reconnected agent.
	connID := s.registry.ConnectionID(nodeID)

	log.Info().Str("node_id", nodeID).Uint64("conn_id", connID).Msg("heartbeat stream opened")

	s.registry.UpdateHeartbeat(first.NodeId, first.Utilization, first.Tasks)

	// ACK goroutine: sends heartbeat responses back to the agent
	done := make(chan struct{})
	defer close(done)

	// Send ACK for the first heartbeat, echoing the sequence number
	if err := stream.Send(&computev1.HeartbeatResponse{
		Acknowledged: true,
		Sequence:     first.Sequence,
	}); err != nil {
		log.Warn().Err(err).Msg("failed to send initial heartbeat ack")
		return err
	}

	// Send periodic keepalive ACKs
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

	// Receive subsequent heartbeats
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

		// Validate subsequent heartbeat BEFORE processing.
		if err := validator.ValidateNodeID(req.NodeId); err != nil {
			logValidationRejection(stream.Context(), "Heartbeat", "node_id", err.Error())
			return err
		}
		if err := validator.ValidateHeartbeatTaskCount(len(req.Tasks)); err != nil {
			logValidationRejection(stream.Context(), "Heartbeat", "tasks", err.Error())
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
	if err := s.checkAuth(ctx); err != nil {
		return nil, err
	}
	if err := validator.ValidateTaskID(req.TaskId); err != nil {
		logValidationRejection(ctx, "ExecuteTask", "task_id", err.Error())
		return nil, err
	}
	if err := validator.ValidateJobID(req.JobId); err != nil {
		logValidationRejection(ctx, "ExecuteTask", "job_id", err.Error())
		return nil, err
	}
	if err := validator.ValidateContainerImage(req.ContainerImage); err != nil {
		logValidationRejection(ctx, "ExecuteTask", "container_image", err.Error())
		return nil, err
	}
	if err := validator.ValidateCommands(req.Command); err != nil {
		logValidationRejection(ctx, "ExecuteTask", "command", err.Error())
		return nil, err
	}
	if err := validator.ValidateEnvVars(req.Env); err != nil {
		logValidationRejection(ctx, "ExecuteTask", "env", err.Error())
		return nil, err
	}
	if err := validator.ValidatePriority(req.Priority); err != nil {
		logValidationRejection(ctx, "ExecuteTask", "priority", err.Error())
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
	if err := s.checkAuth(ctx); err != nil {
		return nil, err
	}
	if err := validator.ValidateAllocationID(req.AllocationId); err != nil {
		logValidationRejection(ctx, "AllocateMemory", "allocation_id", err.Error())
		return nil, err
	}
	if err := validator.ValidateMemorySize(req.SizeBytes); err != nil {
		logValidationRejection(ctx, "AllocateMemory", "size_bytes", err.Error())
		return nil, err
	}
	if err := validator.ValidateReplicationFactor(req.ReplicationFactor); err != nil {
		logValidationRejection(ctx, "AllocateMemory", "replication_factor", err.Error())
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
	if err := s.checkAuth(ctx); err != nil {
		return nil, err
	}
	if err := validator.ValidateAllocationID(req.AllocationId); err != nil {
		logValidationRejection(ctx, "AllocateGPUMemory", "allocation_id", err.Error())
		return nil, err
	}
	if err := validator.ValidateGPUMemorySize(req.SizeBytes); err != nil {
		logValidationRejection(ctx, "AllocateGPUMemory", "size_bytes", err.Error())
		return nil, err
	}
	if err := validator.ValidateGPUDeviceIndex(req.GpuDeviceIndex); err != nil {
		logValidationRejection(ctx, "AllocateGPUMemory", "gpu_device_index", err.Error())
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
	if err := s.checkAuth(ctx); err != nil {
		return nil, err
	}
	if err := validator.ValidateAllocationID(req.AllocationId); err != nil {
		logValidationRejection(ctx, "FreeMemory", "allocation_id", err.Error())
		return nil, err
	}
	log.Info().Str("allocation_id", req.AllocationId).Msg("memory free requested")
	return &computev1.FreeMemoryResponse{Success: true}, nil
}

func (s *AgentServer) FreeGPUMemory(ctx context.Context, req *computev1.FreeGPUMemoryRequest) (*computev1.FreeGPUMemoryResponse, error) {
	if err := s.checkAuth(ctx); err != nil {
		return nil, err
	}
	if err := validator.ValidateAllocationID(req.AllocationId); err != nil {
		logValidationRejection(ctx, "FreeGPUMemory", "allocation_id", err.Error())
		return nil, err
	}
	log.Info().Str("allocation_id", req.AllocationId).Msg("GPU memory free requested")
	return &computev1.FreeGPUMemoryResponse{Success: true}, nil
}

// ============================================================================
// REST API Server
// ============================================================================

// APIServer provides the REST API gateway.
type APIServer struct {
	registry *NodeRegistry
	// API key for REST auth (empty = no auth).
	apiKey string
	// Whether TLS is enabled for the HTTP server (controls HSTS header).
	tlsEnabled bool
}

func (a *APIServer) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {		if a.apiKey != "" {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				key = r.URL.Query().Get("api_key")
			}
			if subtle.ConstantTimeCompare([]byte(key), []byte(a.apiKey)) != 1 {
			setSecurityHeaders(w, a.tlsEnabled)
		w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		}
		next(w, r)
	}
}

func (a *APIServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w, a.tlsEnabled)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *APIServer) nodesHandler(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w, a.tlsEnabled)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(a.registry.GetAll())
}

// setSecurityHeaders adds standard HTTP security headers to every response.
// When TLS is enabled, it also adds the Strict-Transport-Security (HSTS)
// header with a 1-year max-age to instruct browsers to always use HTTPS.
func setSecurityHeaders(w http.ResponseWriter, tlsEnabled bool) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	if tlsEnabled {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

// ============================================================================
// Main
// ============================================================================

func main() {
	var (
		grpcAddr   = flag.String("grpc-addr", ":9000", "gRPC listen address")
		httpAddr   = flag.String("http-addr", ":8080", "REST API listen address")
		nodeID     = flag.String("node-id", "", "Node ID (defaults to hostname)")
		logLevel   = flag.String("log-level", "info", "Log level")
		bootstrap  = flag.Bool("bootstrap", false, "Bootstrap new cluster")
		agentToken = flag.String("agent-token", "", "Shared secret for agent authentication (empty = no auth)")
		apiKey     = flag.String("api-key", "", "API key for REST auth (empty = no auth)")
		tlsCert    = flag.String("tls-cert", "", "Path to TLS server certificate (PEM)")
		tlsKey     = flag.String("tls-key", "", "Path to TLS server private key (PEM)")
		tlsCACert  = flag.String("tls-ca-cert", "", "Path to CA certificate for mTLS (PEM). If set, requires client certs.")
		requireTLS        = flag.Bool("require-tls", false, "Refuse startup unless TLS is configured (production safety)")
		tlsReloadInterval = flag.Duration("tls-reload-interval", 60*time.Second, "Interval for polling TLS cert files for changes (0 = disable hot-reload)")
	)
	flag.Parse()

	level, _ := zerolog.ParseLevel(*logLevel)
	zerolog.SetGlobalLevel(level)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	if *nodeID == "" {
		h, _ := os.Hostname()
		*nodeID = h
	}

	// Generate a cluster ID on bootstrap
	clusterID := "cluster-001"
	if *bootstrap {
		clusterID = fmt.Sprintf("cluster-%s", uuid7())
	}

	// Determine the actual gRPC listen address
	leaderAddr := *grpcAddr
	if leaderAddr == ":9000" || leaderAddr == "" {
		leaderAddr = "localhost:9000"
	}

	log.Info().
		Str("node_id", *nodeID).
		Str("grpc_addr", *grpcAddr).
		Str("http_addr", *httpAddr).
		Bool("bootstrap", *bootstrap).
		Bool("auth_enabled", *agentToken != "" || *apiKey != "").
		Str("cluster_id", clusterID).
		Msg("compute-control-plane starting")

	registry := NewNodeRegistry()

	// Start stale node cleanup goroutine
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	go registry.StaleNodeCleanup(cleanupCtx, 30*time.Second)

	// Start gRPC server
	grpcLis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to listen gRPC")
	}

	// --- TLS setup with hot-reload support ---
	var tlsEnabled bool
	switch {
	case *tlsCert == "" && *tlsKey == "":
		if *requireTLS {
			log.Fatal().Msg("--require-tls is set but no TLS credentials were provided; refusing to start with plaintext")
		}
		tlsEnabled = false
	case *tlsCert == "" || *tlsKey == "":
		log.Fatal().Msg("both --tls-cert and --tls-key must be provided together")
	default:
		tlsEnabled = true
	}

	if !tlsEnabled {
		log.Warn().Msg("TLS not configured — gRPC running with plaintext (insecure)")
	}

	var reloader *tlsreload.CertReloader
	if tlsEnabled {
		var err error
		reloader, err = tlsreload.New(*tlsCert, *tlsKey, *tlsCACert)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to load TLS credentials")
		}

		log.Info().
			Bool("mtls", *tlsCACert != "").
			Dur("reload_interval", *tlsReloadInterval).
			Msg("TLS enabled for gRPC + HTTPS servers")

		// Start background cert file watcher (unless interval is 0).
		if *tlsReloadInterval > 0 {
			go reloader.Watch(context.Background(), *tlsReloadInterval)
		}
	}

	grpcOpts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     5 * time.Minute,
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: 10 * time.Second,
			Time:                  10 * time.Second,
			Timeout:               5 * time.Second,
		}),
		grpc.UnaryInterceptor(metrics.UnaryServerInterceptor()),
		grpc.StreamInterceptor(metrics.StreamServerInterceptor()),
	}
	if tlsEnabled {
		grpcCfg := &tls.Config{
			MinVersion:         tls.VersionTLS13,
			GetConfigForClient: reloader.GetConfigForClient,
		}
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(grpcCfg)))
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	agentServer := &AgentServer{
		registry:   registry,
		clusterID:  clusterID,
		leaderAddr: leaderAddr,
		agentToken: *agentToken,
	}

	computev1.RegisterAgentServiceServer(grpcServer, agentServer)

	go func() {
		log.Info().Str("addr", *grpcAddr).Msg("gRPC server starting")
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatal().Err(err).Msg("gRPC server failed")
		}
	}()

	// Start REST API
	api := &APIServer{
		registry:   registry,
		apiKey:     *apiKey,
		tlsEnabled: tlsEnabled,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.healthHandler)
	mux.HandleFunc("/api/v1/nodes", api.requireAPIKey(api.nodesHandler))
	mux.Handle("/metrics", api.requireAPIKey(metrics.Handler().ServeHTTP))

	httpServer := &http.Server{
		Addr:              *httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if tlsEnabled {
		httpServer.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: reloader.GetCertificate,
		}
	}

	go func() {
		if tlsEnabled {
			log.Info().Str("addr", *httpAddr).Msg("HTTPS API starting")
			// Pass empty cert/key — the server uses TLSConfig.GetCertificate.
			if err := httpServer.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("HTTPS server failed")
			}
		} else {
			log.Info().Str("addr", *httpAddr).Msg("HTTP API starting (plaintext)")
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("HTTP server failed")
			}
		}
	}()

	log.Info().Msg("compute-control-plane initialized and ready")

	// Wait for shutdown or SIGHUP (certificate reload)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
shutdown:
	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGHUP:
			if reloader != nil {
				log.Info().Msg("SIGHUP received, reloading TLS certificates")
				reloader.Reload()
			}
			continue
		default:
			log.Info().Str("signal", sig.String()).Msg("shutting down")
			break shutdown
		}
	}

	grpcServer.GracefulStop()

	// Shutdown HTTP server with a timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn().Err(err).Msg("HTTP server forced to shutdown")
	}

	log.Info().Msg("shutdown complete")
}

func uuid7() string {
	now := time.Now().UnixMilli()
	return fmt.Sprintf("%08x-%04x-7%03x-%04x-%012x",
		uint32(now>>16)&0xFFFFFFFF,
		uint16(now&0xFFFF),
		uint16(now&0xFFF),
		uint16(now>>4)&0xFFFF|0x8000,
		uint64(now)&0xFFFFFFFFFFFF)
}
