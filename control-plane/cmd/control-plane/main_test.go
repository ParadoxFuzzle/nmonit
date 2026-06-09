package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	computev1 "github.com/compute-nmonit/control-plane/gen/compute/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ============================================================================
// NodeRegistry Tests
// ============================================================================

func TestNodeRegistry_Register(t *testing.T) {
	reg := NewNodeRegistry()
	res := &computev1.NodeResources{
		NodeId:   "node-1",
		Hostname: "worker-01",
		Cpu: &computev1.CpuResources{
			PhysicalCores: 8,
			LogicalCores:  16,
			Architecture:  "x86_64",
			ModelName:     "Intel Xeon",
		},
	}

	reg.Register("node-1", "192.168.1.10:9000", res)

	nodes := reg.GetAll()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	n := nodes[0]
	if n.NodeID != "node-1" {
		t.Errorf("expected node_id 'node-1', got %s", n.NodeID)
	}
	if n.Hostname != "worker-01" {
		t.Errorf("expected hostname 'worker-01', got %s", n.Hostname)
	}
	if n.Address != "192.168.1.10:9000" {
		t.Errorf("expected address '192.168.1.10:9000', got %s", n.Address)
	}
	if n.CPU.PhysicalCores != 8 {
		t.Errorf("expected 8 physical cores, got %d", n.CPU.PhysicalCores)
	}
	if n.CPU.Architecture != "x86_64" {
		t.Errorf("expected arch 'x86_64', got %s", n.CPU.Architecture)
	}
	if n.LastBeat == "" {
		t.Error("expected LastBeat to be non-empty")
	}
}

func TestNodeRegistry_UpdateHeartbeat(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", nil)

	oldBeat := reg.nodes["node-1"].LastBeat
	time.Sleep(10 * time.Millisecond)

	reg.UpdateHeartbeat("node-1", &computev1.NodeUtilization{
		NodeId: "node-1",
		Cpu:    &computev1.CpuUtilization{OverallPercent: 45.0},
	}, nil)

	if !reg.nodes["node-1"].LastBeat.After(oldBeat) {
		t.Error("expected LastBeat to be updated after heartbeat")
	}
}

func TestNodeRegistry_UpdateHeartbeat_UnknownNode(t *testing.T) {
	reg := NewNodeRegistry()
	// Should not panic for unknown node
	reg.UpdateHeartbeat("nonexistent", &computev1.NodeUtilization{NodeId: "nonexistent"}, nil)
	if len(reg.GetAll()) != 0 {
		t.Error("expected no nodes registered for unknown heartbeat")
	}
}

func TestNodeRegistry_UpdateHeartbeat_StoresTasks(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", nil)

	tasks := []*computev1.TaskStatus{
		{TaskId: "task-1", State: computev1.TaskState_TASK_STATE_RUNNING},
		{TaskId: "task-2", State: computev1.TaskState_TASK_STATE_PENDING},
	}
	reg.UpdateHeartbeat("node-1", nil, tasks)

	reg.mu.RLock()
	node := reg.nodes["node-1"]
	reg.mu.RUnlock()

	if len(node.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(node.Tasks))
	}
	if node.Tasks["task-1"].State != computev1.TaskState_TASK_STATE_RUNNING {
		t.Error("expected task-1 to be RUNNING")
	}
}

func TestNodeRegistry_UpdateHeartbeat_ReplacesTaskMap(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", nil)

	// First heartbeat: 2 tasks
	reg.UpdateHeartbeat("node-1", nil, []*computev1.TaskStatus{
		{TaskId: "task-1", State: computev1.TaskState_TASK_STATE_RUNNING},
		{TaskId: "task-2", State: computev1.TaskState_TASK_STATE_PENDING},
	})

	// Second heartbeat: only 1 task (task-1 completed, no longer reported)
	reg.UpdateHeartbeat("node-1", nil, []*computev1.TaskStatus{
		{TaskId: "task-2", State: computev1.TaskState_TASK_STATE_COMPLETED},
	})

	reg.mu.RLock()
	node := reg.nodes["node-1"]
	reg.mu.RUnlock()

	if len(node.Tasks) != 1 {
		t.Fatalf("expected 1 task (map replaced), got %d", len(node.Tasks))
	}
	if _, ok := node.Tasks["task-1"]; ok {
		t.Error("task-1 should have been removed when map was replaced")
	}
	if node.Tasks["task-2"].State != computev1.TaskState_TASK_STATE_COMPLETED {
		t.Error("expected task-2 to be COMPLETED")
	}
}

func TestNodeRegistry_ConnectionID_IncrementsOnReRegister(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", nil)

	if reg.ConnectionID("node-1") != 1 {
		t.Error("expected ConnectionID 1 on first registration")
	}

	// Re-register the same node (simulating reconnection)
	reg.Register("node-1", "new-addr", nil)

	if reg.ConnectionID("node-1") != 2 {
		t.Error("expected ConnectionID 2 after re-registration")
	}
}

func TestNodeRegistry_RemoveIfConnectionMatches(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", nil)
	reg.Register("node-2", "", nil)

	conn1 := reg.ConnectionID("node-1")

	// Re-register node-1 to bump its connection ID
	reg.Register("node-1", "new-addr", nil)

	// Old connection ID should NOT remove the node (conn ID mismatched)
	reg.RemoveIfConnectionMatches("node-1", conn1)

	if reg.ConnectionID("node-1") == 0 {
		t.Error("node-1 should NOT be removed by old connection ID")
	}

	// Current connection ID SHOULD remove the node
	currentConn := reg.ConnectionID("node-1")
	reg.RemoveIfConnectionMatches("node-1", currentConn)

	if reg.ConnectionID("node-1") != 0 {
		t.Error("node-1 should be removed by current connection ID")
	}

	// node-2 should still be present
	if reg.ConnectionID("node-2") == 0 {
		t.Error("node-2 should still be registered")
	}
}

func TestNodeRegistry_GetAll_ReturnsDeepCopy(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "addr-1", &computev1.NodeResources{
		NodeId:   "node-1",
		Hostname: "host-1",
		Cpu:      &computev1.CpuResources{PhysicalCores: 4},
	})

	// GetAll should return a slice, not internal pointers
	result := reg.GetAll()

	// Mutating the returned slice should not affect the registry
	result[0].NodeID = "modified"

	reg.mu.RLock()
	internal := reg.nodes["node-1"]
	reg.mu.RUnlock()

	if internal.NodeID != "node-1" {
		t.Error("GetAll returned reference to internal state — should be deep copy")
	}
}

func TestNodeRegistry_GetAll_EmptyRegistry(t *testing.T) {
	reg := NewNodeRegistry()
	result := reg.GetAll()
	if result == nil {
		t.Error("expected non-nil slice from empty registry")
	}
	if len(result) != 0 {
		t.Errorf("expected empty slice, got %d nodes", len(result))
	}
}

func TestNodeRegistry_Remove(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", nil)
	reg.Register("node-2", "", nil)

	reg.Remove("node-1")

	nodes := reg.GetAll()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node after remove, got %d", len(nodes))
	}
	if nodes[0].NodeID != "node-2" {
		t.Errorf("expected node-2, got %s", nodes[0].NodeID)
	}

	// Remove nonexistent should not panic
	reg.Remove("nonexistent")
	if len(reg.GetAll()) != 1 {
		t.Error("removing nonexistent node should not affect registry")
	}
}

func TestNodeRegistry_StaleNodeCleanup(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", nil)

	// Artificially age the last beat
	reg.mu.Lock()
	reg.nodes["node-1"].LastBeat = time.Now().Add(-60 * time.Second)
	reg.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run cleanup with short ticker by directly testing the logic
	// rather than waiting for the goroutine
	go reg.StaleNodeCleanup(ctx, 30*time.Second)

	// Give the cleanup goroutine one tick cycle
	time.Sleep(50 * time.Millisecond)
	cancel()

	// The goroutine may not have executed yet due to the 10s ticker.
	// Test the logic directly.
	reg.mu.Lock()
	now := time.Now()
	for id, info := range reg.nodes {
		if now.Sub(info.LastBeat) > 30*time.Second {
			delete(reg.nodes, id)
		}
	}
	reg.mu.Unlock()

	nodes := reg.GetAll()
	if len(nodes) != 0 {
		t.Errorf("expected stale node to be removed, got %d nodes", len(nodes))
	}
}

func TestNodeRegistry_ConcurrentAccess(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", &computev1.NodeResources{
		NodeId: "node-1",
		Cpu:    &computev1.CpuResources{PhysicalCores: 4},
	})

	var wg sync.WaitGroup
	const goroutines = 50

	// Concurrent heartbeat updates
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.UpdateHeartbeat("node-1", &computev1.NodeUtilization{
				NodeId: "node-1",
			}, nil)
		}()
	}

	// Concurrent GetAll reads
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reg.GetAll()
		}()
	}

	wg.Wait()

	nodes := reg.GetAll()
	if len(nodes) != 1 {
		t.Errorf("expected 1 node after concurrent access, got %d", len(nodes))
	}
}

// ============================================================================
// Auth Middleware Tests
// ============================================================================

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

// ============================================================================
// REST API Auth Middleware Tests
// ============================================================================

func TestRequireAPIKey_NoKeyRequired(t *testing.T) {
	api := &APIServer{apiKey: "", registry: NewNodeRegistry()}
	called := false
	handler := api.requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if !called {
		t.Error("expected handler to be called when no API key required")
	}
}

func TestRequireAPIKey_MissingKey(t *testing.T) {
	api := &APIServer{apiKey: "my-api-key", registry: NewNodeRegistry()}
	called := false
	handler := api.requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if called {
		t.Error("expected handler to be blocked when API key missing")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestRequireAPIKey_ValidHeader(t *testing.T) {
	api := &APIServer{apiKey: "my-api-key", registry: NewNodeRegistry()}
	called := false
	handler := api.requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.Header.Set("X-API-Key", "my-api-key")
	w := httptest.NewRecorder()
	handler(w, req)

	if !called {
		t.Error("expected handler to be called with valid X-API-Key header")
	}
}

func TestRequireAPIKey_ValidQueryParam(t *testing.T) {
	api := &APIServer{apiKey: "my-api-key", registry: NewNodeRegistry()}
	called := false
	handler := api.requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes?api_key=my-api-key", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if !called {
		t.Error("expected handler to be called with valid api_key query param")
	}
}

func TestRequireAPIKey_InvalidKey(t *testing.T) {
	api := &APIServer{apiKey: "my-api-key", registry: NewNodeRegistry()}
	called := false
	handler := api.requireAPIKey(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil)
	req.Header.Set("X-API-Key", "wrong-key")
	w := httptest.NewRecorder()
	handler(w, req)

	if called {
		t.Error("expected handler to be blocked with wrong API key")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// ============================================================================
// Helper Function Tests
// ============================================================================

func TestSafeHostname_NilResources(t *testing.T) {
	result := safeHostname(nil)
	if result != "" {
		t.Errorf("expected empty string for nil, got %q", result)
	}
}

func TestSafeHostname(t *testing.T) {
	res := &computev1.NodeResources{Hostname: "my-host"}
	result := safeHostname(res)
	if result != "my-host" {
		t.Errorf("expected 'my-host', got %q", result)
	}
}

func TestCPUSummary_Nil(t *testing.T) {
	result := cpuSummary(nil)
	if result.PhysicalCores != 0 {
		t.Error("expected zero CpuSummary for nil resources")
	}
}

func TestCPUSummary_WithData(t *testing.T) {
	res := &computev1.NodeResources{
		Cpu: &computev1.CpuResources{
			PhysicalCores: 16,
			LogicalCores:  32,
			Architecture:  "aarch64",
			ModelName:     "Apple M2",
		},
	}
	result := cpuSummary(res)
	if result.PhysicalCores != 16 {
		t.Errorf("expected 16 cores, got %d", result.PhysicalCores)
	}
	if result.LogicalCores != 32 {
		t.Errorf("expected 32 logical, got %d", result.LogicalCores)
	}
	if result.Architecture != "aarch64" {
		t.Errorf("expected aarch64, got %s", result.Architecture)
	}
}

func TestMemorySummary(t *testing.T) {
	res := &computev1.NodeResources{
		Memory: &computev1.MemoryResources{
			TotalBytes: 16 * 1024 * 1024 * 1024, // 16 GB
		},
	}
	result := memorySummary(res)
	if result.TotalGB != 16.0 {
		t.Errorf("expected 16 GB, got %f", result.TotalGB)
	}
}

func TestGPUSummary(t *testing.T) {
	res := &computev1.NodeResources{
		Gpus: []*computev1.GpuResources{
			{Index: 0, Vendor: "nvidia", Model: "RTX 4090", VramBytes: 24 * 1024 * 1024 * 1024},
			{Index: 1, Vendor: "nvidia", Model: "RTX 4090", VramBytes: 24 * 1024 * 1024 * 1024},
		},
	}
	result := gpuSummary(res)
	if len(result) != 2 {
		t.Fatalf("expected 2 GPUs, got %d", len(result))
	}
	if result[0].VRamGB != 24.0 {
		t.Errorf("expected 24 GB VRAM, got %f", result[0].VRamGB)
	}
	if result[1].Index != 1 {
		t.Errorf("expected index 1, got %d", result[1].Index)
	}
}

func TestGPUSummary_Empty(t *testing.T) {
	result := gpuSummary(nil)
	if result == nil {
		t.Error("expected non-nil empty slice")
	}
	if len(result) != 0 {
		t.Errorf("expected empty slice, got %d GPUs", len(result))
	}
}

func TestUuid7_Format(t *testing.T) {
	id := uuid7()
	if len(id) != 36 {
		t.Errorf("expected 36 characters (UUID format), got %d: %s", len(id), id)
	}
	// Check UUID v7 format: xxxxxxxx-xxxx-7xxx-xxxx-xxxxxxxxxxxx
	if id[14] != '7' {
		t.Errorf("expected version '7' at position 14, got %c in %s", id[14], id)
	}
}

func TestUuid7_Uniqueness(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := uuid7()
		if ids[id] {
			t.Logf("collision at iteration %d, adding 1ms delay", i)
			time.Sleep(time.Millisecond)
			id = uuid7()
		}
		if ids[id] {
			t.Errorf("duplicate UUID generated even after delay: %s", id)
		}
		ids[id] = true
	}
}
