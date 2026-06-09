package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/compute-nmonit/control-plane/gen/compute/v1"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// NodeRegistry holds state about connected agent nodes.
type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeInfo
}

type NodeInfo struct {
	NodeID      string
	Address     string
	Resources   *computev1.NodeResources
	LastBeat    time.Time
	Tasks       map[string]*computev1.TaskStatus
}

func (r *NodeRegistry) Register(nodeID string, addr string, res *computev1.NodeResources) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[nodeID] = &NodeInfo{
		NodeID:    nodeID,
		Address:   addr,
		Resources: res,
		LastBeat:  time.Now(),
		Tasks:     make(map[string]*computev1.TaskStatus),
	}
}

func (r *NodeRegistry) UpdateHeartbeat(nodeID string, util *computev1.NodeUtilization, tasks []*computev1.TaskStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok {
		n.LastBeat = time.Now()
		for _, t := range tasks {
			n.Tasks[t.TaskId] = t
		}
	}
}

func (r *NodeRegistry) GetAll() map[string]*NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*NodeInfo, len(r.nodes))
	for k, v := range r.nodes {
		out[k] = v
	}
	return out
}

func (r *NodeRegistry) Remove(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nodes, nodeID)
}

// AgentServer implements the AgentService gRPC server.
type AgentServer struct {
	computev1.UnimplementedAgentServiceServer
	registry *NodeRegistry
	mu       sync.Mutex
}

func (s *AgentServer) Register(ctx context.Context, req *computev1.RegisterRequest) (*computev1.RegisterResponse, error) {
	log.Info().
		Str("node_id", req.NodeId).
		Str("address", req.Address).
		Str("version", req.AgentVersion).
		Msg("node registering")

	s.registry.Register(req.NodeId, req.Address, req.Resources)

	return &computev1.RegisterResponse{
		Accepted:          true,
		ClusterId:         "cluster-001",
		ControlPlaneLeader: "localhost:9000",
		HeartbeatIntervalMs: 5000,
	}, nil
}
func (s *AgentServer) Heartbeat(stream computev1.AgentService_HeartbeatServer) error {
	// Receive first heartbeat to identify the node
	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	nodeID := first.NodeId
	log.Info().Str("node_id", nodeID).Msg("heartbeat stream opened")

	s.registry.UpdateHeartbeat(first.NodeId, first.Utilization, first.Tasks)

	// ACK goroutine: sends heartbeat responses back to the agent
	done := make(chan struct{})
	defer close(done)

	// Send ACK for the first heartbeat
	if err := stream.Send(&computev1.HeartbeatResponse{
		Acknowledged: true,
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

	// Receive subsequent heartbeats
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			log.Info().Str("node_id", nodeID).Msg("heartbeat stream closed")
			return nil
		}
		if err != nil {
			log.Warn().Err(err).Str("node_id", nodeID).Msg("heartbeat stream error")
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
	log.Info().
		Str("task_id", req.TaskId).
		Str("image", req.ContainerImage).
		Msg("task execution requested")
	return &computev1.TaskAck{
		Accepted: true,
	}, nil
}

func (s *AgentServer) AllocateMemory(ctx context.Context, req *computev1.MemoryAllocRequest) (*computev1.MemoryAllocResponse, error) {
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
	log.Info().Str("allocation_id", req.AllocationId).Msg("memory free requested")
	return &computev1.FreeMemoryResponse{Success: true}, nil
}

func (s *AgentServer) FreeGPUMemory(ctx context.Context, req *computev1.FreeGPUMemoryRequest) (*computev1.FreeGPUMemoryResponse, error) {
	log.Info().Str("allocation_id", req.AllocationId).Msg("GPU memory free requested")
	return &computev1.FreeGPUMemoryResponse{Success: true}, nil
}

// APIServer provides the REST API gateway.
type APIServer struct {
	registry *NodeRegistry
}

func (a *APIServer) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *APIServer) nodesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(a.registry.GetAll())
}

func main() {
	var (
		grpcAddr  = flag.String("grpc-addr", ":9000", "gRPC listen address")
		httpAddr  = flag.String("http-addr", ":8080", "REST API listen address")
		nodeID    = flag.String("node-id", "", "Node ID (defaults to hostname)")
		logLevel  = flag.String("log-level", "info", "Log level")
		bootstrap = flag.Bool("bootstrap", false, "Bootstrap new cluster")
	)
	flag.Parse()

	level, _ := zerolog.ParseLevel(*logLevel)
	zerolog.SetGlobalLevel(level)
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	if *nodeID == "" {
		h, _ := os.Hostname()
		*nodeID = h
	}

	log.Info().
		Str("node_id", *nodeID).
		Str("grpc_addr", *grpcAddr).
		Str("http_addr", *httpAddr).
		Bool("bootstrap", *bootstrap).
		Msg("compute-control-plane starting")

	registry := &NodeRegistry{
		nodes: make(map[string]*NodeInfo),
	}

	// Start gRPC server
	grpcLis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to listen gRPC")
	}

	grpcServer := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     5 * time.Minute,
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: 10 * time.Second,
			Time:                  10 * time.Second,
			Timeout:               5 * time.Second,
		}),
	)
	agentServer := &AgentServer{registry: registry}

	// Also register the streaming bidi service
	computev1.RegisterAgentServiceServer(grpcServer, agentServer)

	go func() {
		log.Info().Str("addr", *grpcAddr).Msg("gRPC server starting")
		if err := grpcServer.Serve(grpcLis); err != nil {
			log.Fatal().Err(err).Msg("gRPC server failed")
		}
	}()

	// Start REST API
	api := &APIServer{registry: registry}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", api.healthHandler)
	mux.HandleFunc("/api/v1/nodes", api.nodesHandler)

	httpServer := &http.Server{
		Addr:    *httpAddr,
		Handler: mux,
	}

	go func() {
		log.Info().Str("addr", *httpAddr).Msg("HTTP API starting")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("HTTP server failed")
		}
	}()

	log.Info().Msg("compute-control-plane initialized and ready")

	// Wait for shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Info().Str("signal", sig.String()).Msg("shutting down")

	grpcServer.GracefulStop()
	httpServer.Shutdown(context.Background())
	log.Info().Msg("shutdown complete")
}
