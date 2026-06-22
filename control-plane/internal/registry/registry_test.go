package registry

import (
	"context"
	"sync"
	"testing"
	"time"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
)

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

	all := reg.GetAll()
	if len(all) != 1 {
		t.Fatalf("expected 1 node, got %d", len(all))
	}
	n := all[0]
	if n.NodeID != "node-1" {
		t.Errorf("expected node_id 'node-1', got %s", n.NodeID)
	}
	if n.Address != "192.168.1.10:9000" {
		t.Errorf("expected address '192.168.1.10:9000', got %s", n.Address)
	}
	if n.Hostname != "worker-01" {
		t.Errorf("expected hostname 'worker-01', got %s", n.Hostname)
	}
	if n.CPU == nil || n.CPU.PhysicalCores != 8 {
		t.Errorf("expected 8 physical cores, got %v", n.CPU)
	}
	if n.CPU == nil || n.CPU.Architecture != "x86_64" {
		t.Errorf("expected arch 'x86_64', got %v", n.CPU)
	}
	if n.LastBeat.IsZero() {
		t.Error("expected LastBeat to be populated")
	}
	if n.ConnectionID != 1 {
		t.Errorf("expected ConnectionID 1 on first registration, got %d", n.ConnectionID)
	}
}

func TestNodeRegistry_Register_PanicsOnNilResources(t *testing.T) {
	// Map nil Resources at the input boundary so the stack trace identifies
	// the malformed Register call as the cause rather than a deeper runtime
	// nil-pointer in proto.CloneOf.
	reg := NewNodeRegistry()
	defer func() {
		if recover() == nil {
			t.Error("expected Register to panic on nil Resources, but it did not")
		}
	}()
	reg.Register("bad-node", "addr", nil)
}

func TestNodeRegistry_UpdateHeartbeat(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", &computev1.NodeResources{NodeId: "node-1"})

	oldBeat := reg.nodes["node-1"].LastBeat
	time.Sleep(10 * time.Millisecond)

	reg.UpdateHeartbeat("node-1", &computev1.NodeUtilization{NodeId: "node-1"}, nil)

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
	reg.Register("node-1", "", &computev1.NodeResources{NodeId: "node-1"})

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
	reg.Register("node-1", "", &computev1.NodeResources{NodeId: "node-1"})

	reg.UpdateHeartbeat("node-1", nil, []*computev1.TaskStatus{
		{TaskId: "task-1", State: computev1.TaskState_TASK_STATE_RUNNING},
		{TaskId: "task-2", State: computev1.TaskState_TASK_STATE_PENDING},
	})

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
	reg.Register("node-1", "", &computev1.NodeResources{NodeId: "node-1"})

	if reg.ConnectionID("node-1") != 1 {
		t.Error("expected ConnectionID 1 on first registration")
	}

	reg.Register("node-1", "new-addr", &computev1.NodeResources{NodeId: "node-1"})

	if reg.ConnectionID("node-1") != 2 {
		t.Error("expected ConnectionID 2 after re-registration")
	}
}

func TestNodeRegistry_RemoveIfConnectionMatches(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", &computev1.NodeResources{NodeId: "node-1"})
	reg.Register("node-2", "", &computev1.NodeResources{NodeId: "node-2"})

	conn1 := reg.ConnectionID("node-1")

	reg.Register("node-1", "new-addr", &computev1.NodeResources{NodeId: "node-1"})

	// Old connection ID should NOT remove the node (conn ID mismatched)
	reg.RemoveIfConnectionMatches("node-1", conn1)

	if reg.ConnectionID("node-1") == 0 {
		t.Error("node-1 should NOT be removed by old connection ID")
	}

	currentConn := reg.ConnectionID("node-1")
	reg.RemoveIfConnectionMatches("node-1", currentConn)

	if reg.ConnectionID("node-1") != 0 {
		t.Error("node-1 should be removed by current connection ID")
	}

	if reg.ConnectionID("node-2") == 0 {
		t.Error("node-2 should still be registered")
	}
}

// TestNodeRegistry_GetAll_ReturnsSliceDeepCopy verifies that mutating the
// returned slice's NodeID and ConnectionID does not affect internal state.
func TestNodeRegistry_GetAll_ReturnsSliceDeepCopy(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "addr-1", &computev1.NodeResources{
		NodeId:   "node-1",
		Hostname: "host-1",
		Cpu:      &computev1.CpuResources{PhysicalCores: 4},
	})

	result := reg.GetAll()
	result[0].NodeID = "modified"
	result[0].Hostname = "modified-host"
	result[0].ConnectionID = 999

	reg.mu.RLock()
	internal := reg.nodes["node-1"]
	reg.mu.RUnlock()

	if internal.NodeID != "node-1" {
		t.Error("GetAll returned reference to internal NodeID — should be value-copy")
	}
	if internal.Resources == nil || internal.Resources.Hostname != "host-1" {
		t.Error("host-1 should be unaffected by slice mutation (Hostname is in cloned proto)")
	}
	if internal.ConnectionID != 1 {
		t.Errorf("ConnectionID 1 should be unaffected by slice mutation, got %d", internal.ConnectionID)
	}
}

// TestNodeRegistry_GetAll_ClonesResources verifies that GetAll returns a
// fully-deep-cloned snapshot — every *proto pointer in the returned slice is
// distinct between calls, and a caller mutation of any field on one snapshot
// must not affect another returned snapshot or the registry's internal state.
//
// The table iterates over a single mutation field per case so a regression
// (e.g. a future regression that clones the proto but reuses a nested slice
// pointer) surfaces as one failing subtest rather than masking the cause
// inside a single monolithic test.
func TestNodeRegistry_GetAll_ClonesResources(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "addr-1", &computev1.NodeResources{
		NodeId:   "node-1",
		Hostname: "host-original",
		Cpu:      &computev1.CpuResources{PhysicalCores: 4},
		Gpus: []*computev1.GpuResources{
			{Index: 0, Vendor: "NVIDIA", Model: "A100"},
			{Index: 1, Vendor: "AMD", Model: "MI250"},
		},
	})

	if got := len(reg.GetAll()[0].GPUs); got != 2 {
		t.Fatalf("fixture expected 2 GPUs, got %d", got)
	}

	cases := []struct {
		name   string
		mutate func(s *NodeSummary)
		// checkSnap asserts the second snapshot still holds its original values.
		checkSnap func(t *testing.T, other *NodeSummary)
		// checkInternal asserts registry.nodes["node-1"] still holds its original
		// values. Caller must hold reg.mu.RLock; runs while that lock is held.
		checkInternal func(t *testing.T, internal *NodeInfo)
	}{
		{
			name:   "Cpu.PhysicalCores",
			mutate: func(s *NodeSummary) { s.CPU.PhysicalCores = 99 },
			checkSnap: func(t *testing.T, other *NodeSummary) {
				if other.CPU.PhysicalCores != 4 {
					t.Errorf("expected second snapshot CPU.PhysicalCores=4, got %d", other.CPU.PhysicalCores)
				}
			},
			checkInternal: func(t *testing.T, internal *NodeInfo) {
				if internal.Resources.Cpu.PhysicalCores != 4 {
					t.Errorf("expected internal CPU.PhysicalCores=4, got %d", internal.Resources.Cpu.PhysicalCores)
				}
			},
		},
		{
			name:   "Hostname",
			mutate: func(s *NodeSummary) { s.Hostname = "host-mutated" },
			checkSnap: func(t *testing.T, other *NodeSummary) {
				if other.Hostname != "host-original" {
					t.Errorf("expected second snapshot Hostname=host-original, got %q", other.Hostname)
				}
			},
			checkInternal: func(t *testing.T, internal *NodeInfo) {
				if internal.Resources.Hostname != "host-original" {
					t.Errorf("expected internal Hostname=host-original, got %q", internal.Resources.Hostname)
				}
			},
		},
		{
			name:   "GPUs[0].Vendor",
			mutate: func(s *NodeSummary) { s.GPUs[0].Vendor = "MUTATED" },
			checkSnap: func(t *testing.T, other *NodeSummary) {
				if other.GPUs[0].Vendor != "NVIDIA" {
					t.Errorf("expected second snapshot GPUs[0].Vendor=NVIDIA, got %q", other.GPUs[0].Vendor)
				}
				if other.GPUs[1].Vendor != "AMD" {
					t.Errorf("expected second snapshot GPUs[1].Vendor=AMD, got %q", other.GPUs[1].Vendor)
				}
			},
			checkInternal: func(t *testing.T, internal *NodeInfo) {
				if internal.Resources.Gpus[0].Vendor != "NVIDIA" {
					t.Errorf("expected internal Gpus[0].Vendor=NVIDIA, got %q", internal.Resources.Gpus[0].Vendor)
				}
				if internal.Resources.Gpus[1].Vendor != "AMD" {
					t.Errorf("expected internal Gpus[1].Vendor=AMD, got %q", internal.Resources.Gpus[1].Vendor)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := reg.GetAll()
			second := reg.GetAll()

			if len(first) != 1 || len(second) != 1 {
				t.Fatalf("expected 1 node per call, got %d / %d", len(first), len(second))
			}

			// Pointer-distinctness assertions: every *proto in the returned
			// snapshot must be its own allocation, otherwise proto.Clone has
			// missed a subtree.
			if first[0].CPU == second[0].CPU {
				t.Error("expected distinct *CPU pointers between snapshots")
			}
			if first[0].Memory == nil || second[0].Memory == nil {
				if first[0].Memory != second[0].Memory {
					t.Errorf("inconsistent Memory nil-ness between snapshots: %v vs %v", first[0].Memory, second[0].Memory)
				}
			} else if first[0].Memory == second[0].Memory {
				t.Error("expected distinct *Memory pointers between snapshots")
			}
			if got := len(first[0].GPUs); got != 2 {
				t.Fatalf("expected 2 GPUs in snapshot, got %d", got)
			}
			if first[0].GPUs[0] == second[0].GPUs[0] {
				t.Error("expected distinct *GpuResources pointers for GPUs[0] between snapshots")
			}
			if first[0].GPUs[1] == second[0].GPUs[1] {
				t.Error("expected distinct *GpuResources pointers for GPUs[1] between snapshots")
			}

			// Capture a stable *NodeInfo reference before mutating, so we
			// can dereference its proto pointers later as the "registry
			// internal" baseline.
			reg.mu.RLock()
			internal := reg.nodes["node-1"]
			reg.mu.RUnlock()

			tc.mutate(&first[0])

			tc.checkSnap(t, &second[0])
			reg.mu.RLock()
			tc.checkInternal(t, internal)
			reg.mu.RUnlock()
		})
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
	reg.Register("node-1", "", &computev1.NodeResources{NodeId: "node-1"})
	reg.Register("node-2", "", &computev1.NodeResources{NodeId: "node-2"})

	reg.Remove("node-1")

	nodes := reg.GetAll()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node after remove, got %d", len(nodes))
	}
	if nodes[0].NodeID != "node-2" {
		t.Errorf("expected node-2, got %s", nodes[0].NodeID)
	}

	reg.Remove("nonexistent")
	if len(reg.GetAll()) != 1 {
		t.Error("removing nonexistent node should not affect registry")
	}
}

func TestNodeRegistry_StaleNodeCleanup(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register("node-1", "", &computev1.NodeResources{NodeId: "node-1"})

	reg.mu.Lock()
	reg.nodes["node-1"].LastBeat = time.Now().Add(-60 * time.Second)
	reg.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go reg.StaleNodeCleanup(ctx, 30*time.Second)

	time.Sleep(50 * time.Millisecond)
	cancel()

	// The goroutine may not have executed yet (10s ticker). Test the
	// eviction logic directly so we don't depend on timing.
	reg.mu.Lock()
	now := time.Now()
	for id, info := range reg.nodes {
		if now.Sub(info.LastBeat) > 30*time.Second {
			delete(reg.nodes, id)
		}
	}
	reg.mu.Unlock()

	if len(reg.GetAll()) != 0 {
		t.Errorf("expected stale node to be removed, got %d nodes", len(reg.GetAll()))
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

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.UpdateHeartbeat("node-1", &computev1.NodeUtilization{NodeId: "node-1"}, nil)
		}()
	}

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reg.GetAll()
		}()
	}

	wg.Wait()

	if len(reg.GetAll()) != 1 {
		t.Errorf("expected 1 node after concurrent access, got %d", len(reg.GetAll()))
	}
}
