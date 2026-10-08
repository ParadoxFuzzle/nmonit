package jobs

import (
	"context"
	"sync"
	"time"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/audit"
	"github.com/ParadoxFuzzle/control-plane/internal/registry"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// Hub buffers work destined for individual agents. The control plane cannot
// call agents directly (agents connect outbound), so TaskSpecs ride the
// Heartbeat stream: each agent's stream drains its hub queue into
// HeartbeatResponse.pending_tasks / cancel_task_ids. The Rust agent already
// consumes both (agent/src/heartbeat.rs).
//
// The hub is also the control-plane-side task ledger for the active window:
// it maps dispatched task ids back to job ids so heartbeat TaskStatus
// reports can transition job states.
type Hub struct {
	mu      sync.Mutex
	pending map[string][]*computev1.PendingTask // node id -> FIFO queue
	cancels map[string][]string                 // node id -> task ids to kill
	taskJob map[string]string                   // task id -> job id
	store   Store
	aud     *audit.Audit
}

// NewHub creates a Hub. store/audit may be nil (dispatch still works, but
// job-state tracking and audit entries are skipped).
func NewHub(store Store, aud *audit.Audit) *Hub {
	return &Hub{
		pending: map[string][]*computev1.PendingTask{},
		cancels: map[string][]string{},
		taskJob: map[string]string{},
		store:   store,
		aud:     aud,
	}
}

// Enqueue queues a TaskSpec for delivery to nodeID on its next heartbeat.
func (h *Hub) Enqueue(nodeID string, spec *computev1.TaskSpec, jobID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending[nodeID] = append(h.pending[nodeID], &computev1.PendingTask{Spec: spec})
	if jobID != "" {
		h.taskJob[spec.TaskId] = jobID
	}
}

// EnqueueCancel queues a task cancellation for nodeID.
func (h *Hub) EnqueueCancel(nodeID, taskID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cancels[nodeID] = append(h.cancels[nodeID], taskID)
}

// Drain returns and clears the pending tasks queued for nodeID.
func (h *Hub) Drain(nodeID string) []*computev1.PendingTask {
	h.mu.Lock()
	defer h.mu.Unlock()
	tasks := h.pending[nodeID]
	delete(h.pending, nodeID)
	return tasks
}

// DrainCancels returns and clears the cancel ids queued for nodeID.
func (h *Hub) DrainCancels(nodeID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := h.cancels[nodeID]
	delete(h.cancels, nodeID)
	return ids
}

// JobForTask returns the job id owning taskID, if dispatched by this hub.
func (h *Hub) JobForTask(taskID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.taskJob[taskID]
}

// ApplyTaskStatuses transitions job state from agent-reported task states.
// Called from the heartbeat stream for every reported task. Unknown task
// ids (agent-internal tasks not dispatched by the hub) are ignored.
func (h *Hub) ApplyTaskStatuses(ctx context.Context, nodeID string, tasks []*computev1.TaskStatus) {
	if h.store == nil {
		return
	}
	for _, t := range tasks {
		if h.JobForTask(t.TaskId) == "" {
			continue // not ours (or already forgotten)
		}
		var (
			state  State
			errMsg string
		)
		switch t.State {
		case computev1.TaskState_TASK_STATE_RUNNING:
			state = StateRunning
		case computev1.TaskState_TASK_STATE_COMPLETED:
			state = StateCompleted
		case computev1.TaskState_TASK_STATE_FAILED:
			state = StateFailed
			errMsg = t.ErrorMessage
		case computev1.TaskState_TASK_STATE_CANCELLED:
			state = StateCancelled
		case computev1.TaskState_TASK_STATE_PREEMPTED:
			state = StateScheduling
		default:
			continue // PENDING/UNKNOWN: no transition
		}
		if err := h.store.UpdateStateByTask(ctx, t.TaskId, state, errMsg); err != nil {
			log.Warn().Err(err).Str("task_id", t.TaskId).Str("state", string(state)).
				Msg("jobs: task status update failed")
			continue
		}
		if state == StateCompleted || state == StateFailed || state == StateCancelled {
			h.forgetTask(t.TaskId)
			h.auditJob(ctx, t.TaskId, string(state))
		}
		log.Info().Str("node_id", nodeID).Str("task_id", t.TaskId).
			Str("state", string(state)).Msg("job task status updated")
	}
}

func (h *Hub) forgetTask(taskID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.taskJob, taskID)
}

func (h *Hub) auditJob(ctx context.Context, taskID, outcome string) {
	if h.aud == nil || h.store == nil {
		return
	}
	j, err := h.store.GetByTaskID(ctx, taskID)
	if err != nil {
		return
	}
	h.aud.Append(ctx, audit.Entry{
		Actor: "system", Action: "job.state", Object: j.ID,
		Detail: map[string]any{"name": j.Name, "node": j.NodeID}, Outcome: outcome,
	})
}

// --- Scheduler ---

// Scheduler is a minimal best-fit dispatcher for SIMPLE jobs: on each pass
// it takes the oldest queued jobs and assigns each to the online node with
// the fewest running tasks that fits the requested cpu/mem. Dispatch
// delivery happens via the hub on the node's next heartbeat.
type Scheduler struct {
	hub            *Hub
	reg            *registry.NodeRegistry
	store          Store
	aud            *audit.Audit
	maxJobCores    float32
	maxJobMemBytes uint64
	// staleAfter bounds how long since a node's last heartbeat before it is
	// considered offline for scheduling.
	staleAfter time.Duration
}

// NewScheduler wires a Scheduler. reg must be the live registry; hub the
// dispatch hub sharing the same store.
func NewScheduler(hub *Hub, reg *registry.NodeRegistry, store Store, aud *audit.Audit,
	maxJobCores float32, maxJobMemBytes uint64) *Scheduler {
	return &Scheduler{
		hub:            hub,
		reg:            reg,
		store:          store,
		aud:            aud,
		maxJobCores:    maxJobCores,
		maxJobMemBytes: maxJobMemBytes,
		staleAfter:     20 * time.Second,
	}
}

// ScheduleOnce tries to dispatch up to limit queued jobs. Callers: the REST
// submit path (immediate attempt) and the heartbeat stream (opportunistic
// retry when a node reports in). Returns the number of jobs dispatched.
func (s *Scheduler) ScheduleOnce(ctx context.Context, limit int) int {
	if s == nil || s.store == nil {
		return 0
	}
	queued, err := s.store.ListQueued(ctx, limit)
	if err != nil {
		log.Error().Err(err).Msg("jobs: scheduler list queued failed")
		return 0
	}
	dispatched := 0
	for _, j := range queued {
		if j.CPUCores > s.maxJobCores || j.MemBytes > s.maxJobMemBytes {
			// Should have been caught at submit; belt-and-braces here.
			_ = s.store.UpdateState(ctx, j.ID, StateFailed, "resource request exceeds per-job limits")
			continue
		}
		nodeID := s.pickNode(j)
		if nodeID == "" {
			continue // nothing fits / online; stays queued
		}
		spec := buildTaskSpec(j)
		if err := s.store.SetAssignment(ctx, j.ID, nodeID, spec.TaskId); err != nil {
			continue // lost the race with another scheduler pass
		}
		s.hub.Enqueue(nodeID, spec, j.ID)
		dispatched++
		log.Info().Str("job_id", j.ID).Str("node_id", nodeID).
			Str("task_id", spec.TaskId).Msg("job dispatched")
		if s.aud != nil {
			s.aud.Append(ctx, audit.Entry{
				Actor: "system", Action: "job.dispatch", Object: j.ID,
				Detail: map[string]any{"name": j.Name, "node": nodeID}, Outcome: "ok",
			})
		}
	}
	return dispatched
}

// pickNode returns the best-fit online node for j, or "" if none fits.
// Best-fit = fewest currently-running tasks among nodes with enough cpu/mem.
func (s *Scheduler) pickNode(j Job) string {
	summaries := s.reg.GetAll()
	var best string
	bestLoad := -1
	for _, n := range summaries {
		if time.Since(n.LastBeat) > s.staleAfter {
			continue // stale; not schedulable
		}
		if n.CPU == nil || n.Memory == nil {
			continue
		}
		if float32(n.CPU.LogicalCores) < j.CPUCores || n.Memory.TotalBytes < j.MemBytes {
			continue
		}
		load := s.reg.TaskCount(n.NodeID)
		if bestLoad == -1 || load < bestLoad {
			best, bestLoad = n.NodeID, load
		}
	}
	return best
}

func buildTaskSpec(j Job) *computev1.TaskSpec {
	env := map[string]string{}
	for k, v := range j.Env {
		env[k] = v
	}
	return &computev1.TaskSpec{
		TaskId:         "tsk-" + uuid.NewString(),
		JobId:          j.ID,
		ContainerImage: j.Image,
		Command:        j.Command,
		Env:            env,
		Requirements: &computev1.ResourceRequirements{
			CpuCores:    j.CPUCores,
			MemoryBytes: j.MemBytes,
		},
	}
}
