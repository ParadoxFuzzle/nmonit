// Package jobs implements the Phase 2 consumer job flow: a Postgres-backed
// job store, per-user job quotas, and a minimal best-fit scheduler that
// dispatches TaskSpecs to agents via their heartbeat streams.
//
// Design (agreed for Phase 2):
//   - Submission via REST /api/v1/jobs (primary) and gRPC JobService.
//   - Consumers authenticate with the same nmt_ tokens (role-aware:
//     consumer tokens submit jobs but cannot register nodes; provider
//     tokens register nodes but cannot submit jobs; owner does both).
//   - Quotas: max running jobs, max queued jobs, and per-job resource caps
//     (cores / memory). Per-user overrides live in users.max_jobs_*;
//     NULL falls back to control-plane flag defaults.
//   - Scheduling: SIMPLE jobs only; best-fit online node with capacity.
//     DAG / GANG / SERVICE job types are rejected as not yet supported.
package jobs

import (
	"context"
	"errors"
	"time"
)

// State is the lifecycle state of a job (mirrors computev1.JobState,
// stored as text in Postgres).
type State string

const (
	StateQueued     State = "queued"
	StateScheduling State = "scheduling"
	StateRunning    State = "running"
	StateCompleted  State = "completed"
	StateFailed     State = "failed"
	StateCancelled  State = "cancelled"
)

// ActiveStates are the states that count against a user's running quota.
// Scheduling jobs are on their way to a node; they count as running for
// quota purposes so a flood of submissions cannot over-commit the pool.
var ActiveStates = []State{StateScheduling, StateRunning}

// Job is a persisted consumer job.
type Job struct {
	ID         string
	UserID     string
	Name       string
	Image      string // OCI container image reference
	Command    []string
	Env        map[string]string
	CPUCores   float32
	MemBytes   uint64
	State      State
	NodeID     string // assigned node ("" while queued)
	TaskID     string // dispatched task id ("" until dispatch)
	Error      string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

// QuotaDefaults are the control-plane flag defaults applied when a user has
// no per-user override (users.max_jobs_* IS NULL).
type QuotaDefaults struct {
	MaxRunning     int
	MaxQueued      int
	MaxJobCores    float32
	MaxJobMemBytes uint64
}

// Errors returned by the job store. Callers should use errors.Is.
var (
	ErrNotFound        = errors.New("jobs: not found")
	ErrAlreadyExists   = errors.New("jobs: already exists")
	ErrConflict        = errors.New("jobs: state conflict")
	ErrQuotaQueued     = errors.New("jobs: queued-job quota exceeded")
	ErrQuotaRunning    = errors.New("jobs: running-job quota exceeded")
	ErrQuotaResources  = errors.New("jobs: per-job resource limit exceeded")
	ErrUnsupportedType = errors.New("jobs: job type not supported yet")
)

// Store is the persistence interface for jobs. PGStore is the production
// implementation; MemStore backs unit tests. All methods are safe for
// concurrent use.
type Store interface {
	// Create persists a new job in StateQueued.
	Create(ctx context.Context, j Job) (Job, error)
	// Get returns a job by id.
	Get(ctx context.Context, id string) (Job, error)
	// GetByTaskID returns the job that owns a dispatched task.
	GetByTaskID(ctx context.Context, taskID string) (Job, error)
	// ListByUser returns the user's jobs, newest first.
	ListByUser(ctx context.Context, userID string, limit int) ([]Job, error)
	// ListAll returns all jobs, newest first (owner view).
	ListAll(ctx context.Context, limit int) ([]Job, error)
	// ListQueued returns queued jobs oldest-first (scheduler input).
	ListQueued(ctx context.Context, limit int) ([]Job, error)
	// CountByState counts a user's jobs in one state.
	CountByState(ctx context.Context, userID string, s State) (int, error)
	// SetAssignment records dispatch: node + task id, state -> scheduling.
	SetAssignment(ctx context.Context, id, nodeID, taskID string) error
	// UpdateState transitions a job; timestamps are set for terminal and
	// running transitions. Returns ErrConflict if the job is already in a
	// terminal state (completed/failed/cancelled) and s is different.
	UpdateState(ctx context.Context, id string, s State, errMsg string) error
	// UpdateStateByTask is UpdateState keyed on the dispatched task id.
	UpdateStateByTask(ctx context.Context, taskID string, s State, errMsg string) error
	// ClearAssignment un-assigns a job (e.g. preempted / node vanished),
	// returning it to StateQueued for re-dispatch.
	ClearAssignment(ctx context.Context, id string) error
}
