package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "embed"

	"github.com/google/uuid"
)

//go:embed schema/002_jobs.sql
var schemaSQL string

// PGStore is the PostgreSQL-backed job Store. It shares the identity
// database but owns its own schema file (002_jobs.sql), applied once per
// process and idempotent.
type PGStore struct {
	db *sql.DB
}

// NewPGStore opens a job store on an existing connection pool and applies
// the embedded schema (idempotent), mirroring PGStore in internal/identity.
func NewPGStore(db *sql.DB) (*PGStore, error) {
	p := &PGStore{db: db}
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("jobs: migrate: %w", err)
	}
	return p, nil
}

func (p *PGStore) Create(ctx context.Context, j Job) (Job, error) {
	if j.ID == "" {
		j.ID = uuid.NewString()
	}
	j.State = StateQueued
	j.CreatedAt = time.Now()
	// command and env are NOT NULL columns with '{}' defaults that only apply
	// when omitted; explicit nils from JSON zero-value requests would
	// insert NULL and fail, so normalize to empty values here.
	if j.Command == nil {
		j.Command = []string{}
	}
	if j.Env == nil {
		j.Env = map[string]string{}
	}
	// user_id is a nullable uuid column; owner-submitted jobs have no user
	// and must be stored as NULL (an empty string fails the uuid cast).
	var userID any
	if j.UserID != "" {
		userID = j.UserID
	}
	cmdJSON, err := json.Marshal(j.Command)
	if err != nil {
		return Job{}, fmt.Errorf("jobs: create: marshal command: %w", err)
	}
	envJSON, err := json.Marshal(j.Env)
	if err != nil {
		return Job{}, fmt.Errorf("jobs: create: marshal env: %w", err)
	}
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO jobs (id, user_id, name, image, command, env, cpu_cores, mem_bytes, state, created_at)
		 VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,now())`,
		j.ID, userID, j.Name, j.Image, string(cmdJSON), string(envJSON), j.CPUCores, int64(j.MemBytes), string(j.State))
	if err != nil {
		return Job{}, fmt.Errorf("jobs: create: %w", err)
	}
	return j, nil
}

const jobCols = `id, user_id, name, image, command, env, cpu_cores, mem_bytes,
	state, node_id, task_id, error, created_at, started_at, finished_at`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var userID, state, nodeID, taskID, errMsg sql.NullString
	var mem int64
	var started, finished sql.NullTime
	var cmdJSON, envJSON []byte
	err := row.Scan(&j.ID, &userID, &j.Name, &j.Image, &cmdJSON, &envJSON,
		&j.CPUCores, &mem, &state, &nodeID, &taskID, &errMsg,
		&j.CreatedAt, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	if err := json.Unmarshal(cmdJSON, &j.Command); err != nil {
		return Job{}, fmt.Errorf("jobs: scan: unmarshal command: %w", err)
	}
	if err := json.Unmarshal(envJSON, &j.Env); err != nil {
		return Job{}, fmt.Errorf("jobs: scan: unmarshal env: %w", err)
	}
	j.MemBytes = uint64(mem)
	if userID.Valid {
		j.UserID = userID.String
	}
	if state.Valid {
		j.State = State(state.String)
	}
	if nodeID.Valid {
		j.NodeID = nodeID.String
	}
	if taskID.Valid {
		j.TaskID = taskID.String
	}
	if errMsg.Valid {
		j.Error = errMsg.String
	}
	if started.Valid {
		j.StartedAt = started.Time
	}
	if finished.Valid {
		j.FinishedAt = finished.Time
	}
	return j, nil
}

func (p *PGStore) Get(ctx context.Context, id string) (Job, error) {
	row := p.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id=$1`, id)
	return scanJob(row)
}

func (p *PGStore) GetByTaskID(ctx context.Context, taskID string) (Job, error) {
	row := p.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE task_id=$1`, taskID)
	return scanJob(row)
}

func (p *PGStore) ListByUser(ctx context.Context, userID string, limit int) ([]Job, error) {
	return p.list(ctx, `SELECT `+jobCols+` FROM jobs WHERE user_id=$1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
}

func (p *PGStore) ListAll(ctx context.Context, limit int) ([]Job, error) {
	return p.list(ctx, `SELECT `+jobCols+` FROM jobs ORDER BY created_at DESC LIMIT $1`, limit)
}

func (p *PGStore) ListQueued(ctx context.Context, limit int) ([]Job, error) {
	return p.list(ctx, `SELECT `+jobCols+` FROM jobs WHERE state='queued' ORDER BY created_at LIMIT $1`, limit)
}

func (p *PGStore) list(ctx context.Context, q string, args ...any) ([]Job, error) {
	rows, err := p.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (p *PGStore) CountByState(ctx context.Context, userID string, s State) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx,
		`SELECT count(*) FROM jobs WHERE user_id=$1 AND state=$2`, userID, string(s)).Scan(&n)
	return n, err
}

func (p *PGStore) SetAssignment(ctx context.Context, id, nodeID, taskID string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE jobs SET node_id=$2, task_id=$3, state='scheduling'
		 WHERE id=$1 AND state='queued'`, id, nodeID, taskID)
	if err != nil {
		return err
	}
	return requireAffected(res, ErrConflict)
}

func (p *PGStore) UpdateState(ctx context.Context, id string, s State, errMsg string) error {
	res, err := p.db.ExecContext(ctx, `UPDATE jobs SET state=$2, error=$3,
		started_at=CASE WHEN $2='running' AND started_at IS NULL THEN now() ELSE started_at END,
		finished_at=CASE WHEN $2 IN ('completed','failed','cancelled') THEN now() ELSE finished_at END
		WHERE id=$1 AND NOT (state IN ('completed','failed','cancelled') AND state <> $2)`,
		id, string(s), errMsg)
	if err != nil {
		return err
	}
	return requireAffected(res, ErrNotFound)
}

func (p *PGStore) UpdateStateByTask(ctx context.Context, taskID string, s State, errMsg string) error {
	res, err := p.db.ExecContext(ctx, `UPDATE jobs SET state=$2, error=$3,
		started_at=CASE WHEN $2='running' AND started_at IS NULL THEN now() ELSE started_at END,
		finished_at=CASE WHEN $2 IN ('completed','failed','cancelled') THEN now() ELSE finished_at END
		WHERE task_id=$1 AND NOT (state IN ('completed','failed','cancelled') AND state <> $2)`,
		taskID, string(s), errMsg)
	if err != nil {
		return err
	}
	return requireAffected(res, ErrNotFound)
}

func (p *PGStore) ClearAssignment(ctx context.Context, id string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE jobs SET node_id=NULL, task_id=NULL, state='queued'
		 WHERE id=$1 AND state='scheduling'`, id)
	if err != nil {
		return err
	}
	return requireAffected(res, ErrConflict)
}

func requireAffected(res sql.Result, notFoundErr error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return notFoundErr
	}
	return nil
}

// --- MemStore (unit tests) ---

// MemStore is an in-memory Store for unit tests.
type MemStore struct {
	mu   sync.Mutex
	jobs map[string]Job
}

// NewMemStore returns an empty in-memory job Store.
func NewMemStore() *MemStore { return &MemStore{jobs: map[string]Job{}} }

func (m *MemStore) Create(_ context.Context, j Job) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.ID == "" {
		j.ID = uuid.NewString()
	}
	j.State = StateQueued
	j.CreatedAt = time.Now()
	m.jobs[j.ID] = j
	return j, nil
}

func (m *MemStore) Get(_ context.Context, id string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return j, nil
}

func (m *MemStore) GetByTaskID(_ context.Context, taskID string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.TaskID == taskID {
			return j, nil
		}
	}
	return Job{}, ErrNotFound
}

func (m *MemStore) ListByUser(_ context.Context, userID string, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.jobs {
		if j.UserID == userID {
			out = append(out, j)
		}
	}
	sortJobsNewestFirst(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) ListAll(_ context.Context, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.jobs {
		out = append(out, j)
	}
	sortJobsNewestFirst(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) ListQueued(_ context.Context, limit int) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.jobs {
		if j.State == StateQueued {
			out = append(out, j)
		}
	}
	// Oldest first for FIFO scheduling.
	for i := 0; i < len(out); i++ {
		for k := i + 1; k < len(out); k++ {
			if out[k].CreatedAt.Before(out[i].CreatedAt) {
				out[i], out[k] = out[k], out[i]
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) CountByState(_ context.Context, userID string, s State) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, j := range m.jobs {
		if j.UserID == userID && j.State == s {
			n++
		}
	}
	return n, nil
}

func (m *MemStore) SetAssignment(_ context.Context, id, nodeID, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return ErrNotFound
	}
	if j.State != StateQueued {
		return ErrConflict
	}
	j.NodeID, j.TaskID, j.State = nodeID, taskID, StateScheduling
	m.jobs[id] = j
	return nil
}

func terminal(s State) bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

func (m *MemStore) UpdateState(_ context.Context, id string, s State, errMsg string) error {
	return m.updateState(func(j *Job) bool { return j.ID == id }, s, errMsg)
}

func (m *MemStore) UpdateStateByTask(_ context.Context, taskID string, s State, errMsg string) error {
	return m.updateState(func(j *Job) bool { return j.TaskID == taskID }, s, errMsg)
}

func (m *MemStore) updateState(match func(*Job) bool, s State, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, j := range m.jobs {
		if !match(&j) {
			continue
		}
		if terminal(j.State) && j.State != s {
			return ErrConflict
		}
		now := time.Now()
		switch s {
		case StateRunning:
			if j.StartedAt.IsZero() {
				j.StartedAt = now
			}
		case StateCompleted, StateFailed, StateCancelled:
			j.FinishedAt = now
		}
		j.State, j.Error = s, errMsg
		m.jobs[id] = j
		return nil
	}
	return ErrNotFound
}

func (m *MemStore) ClearAssignment(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return ErrNotFound
	}
	if j.State != StateScheduling {
		return ErrConflict
	}
	j.NodeID, j.TaskID, j.State = "", "", StateQueued
	m.jobs[id] = j
	return nil
}

func sortJobsNewestFirst(jobs []Job) {
	for i := 0; i < len(jobs); i++ {
		for k := i + 1; k < len(jobs); k++ {
			if jobs[k].CreatedAt.After(jobs[i].CreatedAt) {
				jobs[i], jobs[k] = jobs[k], jobs[i]
			}
		}
	}
}

// Compile-time assertions.
var (
	_ Store = (*PGStore)(nil)
	_ Store = (*MemStore)(nil)
)
