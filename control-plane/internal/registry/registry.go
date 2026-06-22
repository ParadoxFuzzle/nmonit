// Package registry tracks the set of agent nodes connected to the control plane.
//
// The registry is the source of truth for node identity, hardware resources,
// heartbeat liveness, active tasks, and connection monotonic IDs.
//
// It is intentionally transport-agnostic: GetAll returns []*NodeInfo (deep
// copies) without JSON tags, so HTTP/REST and any future transport (CLI,
// GraphQL, etc.) can map them into whatever shape they need.
package registry

import (
	"context"
	"fmt"
	"sync"
	"time"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/metrics"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/proto"
)

// NodeInfo is the internal, mutable representation of a registered agent node.
// Each registered agent has one *NodeInfo that lives in registry.nodes and is
// updated in place by Register / UpdateHeartbeat. It is not returned to callers;
// use GetAll() to obtain a deep-copied NodeSummary view.
type NodeInfo struct {
	NodeID       string
	Address      string
	Resources    *computev1.NodeResources
	LastBeat     time.Time
	Tasks        map[string]*computev1.TaskStatus
	ConnectionID uint64 // Monotonic counter; heartbeat stream validates against this
}

// NodeSummary is the read-only, transport-friendly view of a registered node
// returned by GetAll. Proto fields are deep-cloned via proto.Clone on each call
// so callers cannot mutate registry state by holding onto the returned
// structs. The Tasks map (and any internal bookkeeping it implies) is kept
// solely on NodeInfo and is intentionally omitted here.
type NodeSummary struct {
	NodeID       string
	Hostname     string
	Address      string
	LastBeat     time.Time
	CPU          *computev1.CpuResources
	Memory       *computev1.MemoryResources
	GPUs         []*computev1.GpuResources
	ConnectionID uint64
}

// NodeRegistry holds state about connected agent nodes.
type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeInfo
}

// NewNodeRegistry returns an empty registry ready for use.
func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{
		nodes: make(map[string]*NodeInfo),
	}
}

// Register adds or replaces a node, bumping its ConnectionID on re-registration
// so old heartbeat streams don't accidentally remove a reconnected node.
//
// Panics if res is nil. Every registered node must carry NodeResources because
// GetAll returns deep-cloned copies (via proto.CloneOf) and surfaces them to
// REST/gRPC consumers. Failing loudly here surfaces a malformed Register
// request at the input boundary with a clean stack trace; the alternative —
// letting nil reach GetAll — manifests only as a much deeper runtime panic
// when proto.CloneOf blows up on a nil receiver.
func (r *NodeRegistry) Register(nodeID string, addr string, res *computev1.NodeResources) {
	if res == nil {
		panic(fmt.Sprintf("NodeRegistry.Register: nil NodeResources for node_id=%q", nodeID))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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

// UpdateHeartbeat refreshes LastBeat on a node and replaces its task map with
// the agent's current view. Replacing (not appending) prevents unbound memory
// growth from accumulating completed tasks.
func (r *NodeRegistry) UpdateHeartbeat(nodeID string, _ *computev1.NodeUtilization, tasks []*computev1.TaskStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok {
		n.LastBeat = time.Now()
		n.Tasks = make(map[string]*computev1.TaskStatus, len(tasks))
		for _, t := range tasks {
			n.Tasks[t.TaskId] = t
		}
	}
	metrics.HeartbeatsTotal.Inc()
	metrics.TasksRunning.Set(float64(r.totalTasksLocked()))
}

// GetAll returns a deep-copied snapshot of the registered nodes as NodeSummary.
//
// The CPU/Memory/GPUs proto fields are cloned via proto.Clone on each call
// so no caller-held reference points back into registry.nodes. The internal
// Tasks map is intentionally not exposed here.
//
// GetAll is safe under concurrent Register / UpdateHeartbeat because it holds
// r.mu.RLock for the duration of the copy. RLock allows multiple concurrent
// readers (REST handlers, future transports) but blocks writers.
func (r *NodeRegistry) GetAll() []NodeSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NodeSummary, 0, len(r.nodes))
	for _, v := range r.nodes {
		var hostname string
		var cpu *computev1.CpuResources
		var mem *computev1.MemoryResources
		var gpus []*computev1.GpuResources
		if v.Resources != nil {
			cl := proto.CloneOf[*computev1.NodeResources](v.Resources)
			hostname = cl.Hostname
			cpu = cl.Cpu
			mem = cl.Memory
			gpus = cl.Gpus
		}
		out = append(out, NodeSummary{
			NodeID:       v.NodeID,
			Hostname:     hostname,
			Address:      v.Address,
			LastBeat:     v.LastBeat,
			CPU:          cpu,
			Memory:       mem,
			GPUs:         gpus,
			ConnectionID: v.ConnectionID,
		})
	}
	return out
}

// Remove deletes a node by ID. No-op if the ID is unknown.
func (r *NodeRegistry) Remove(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nodes, nodeID)
	metrics.RegisteredNodes.Set(float64(len(r.nodes)))
	metrics.TasksRunning.Set(float64(r.totalTasksLocked()))
}

// RemoveIfConnectionMatches only deletes the node if its connection ID still
// matches. This prevents old heartbeat streams from removing a node that has
// already reconnected under a newer stream.
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
// within the threshold. Run in a background goroutine; cancellable via ctx.
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
