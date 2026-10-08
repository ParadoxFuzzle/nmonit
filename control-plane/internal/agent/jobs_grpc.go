package agent

import (
	"context"
	"errors"

	computev1 "github.com/ParadoxFuzzle/control-plane/gen/compute/v1"
	"github.com/ParadoxFuzzle/control-plane/internal/audit"
	"github.com/ParadoxFuzzle/control-plane/internal/identity"
	"github.com/ParadoxFuzzle/control-plane/internal/jobs"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// JobServer implements compute.v1.JobService (Phase 2). It shares the
// AgentService's Authenticator so the same nmt_ tokens work, with
// role-aware authorization:
//   - bootstrap token / owner role: full access (all jobs, no quotas)
//   - consumer role: submit + manage own jobs (quotas enforced)
//   - provider role: denied (providers contribute compute, not jobs)
type JobServer struct {
	computev1.UnimplementedJobServiceServer
	auth  *Authenticator
	jobs  jobs.Store
	ids   identity.Store // for per-user quota overrides (may be nil)
	hub   *jobs.Hub
	sched *jobs.Scheduler
	aud   *audit.Audit
	defs  jobs.QuotaDefaults
}

// NewJobServer wires a JobServer. auth/jobs are required; the rest may be
// nil where noted.
func NewJobServer(auth *Authenticator, js jobs.Store, ids identity.Store,
	hub *jobs.Hub, sched *jobs.Scheduler, aud *audit.Audit, defs jobs.QuotaDefaults) *JobServer {
	return &JobServer{auth: auth, jobs: js, ids: ids, hub: hub, sched: sched, aud: aud, defs: defs}
}

// check resolves the caller and gates provider-role tokens out of the job
// API. Returns the identity (nil for bootstrap) or a gRPC status error.
func (s *JobServer) check(ctx context.Context) (*Identity, error) {
	if s.auth == nil {
		return nil, status.Error(codes.Unauthenticated, "job API requires authentication")
	}
	authCtx, err := s.auth.Check(ctx, "job-rpc")
	if err != nil {
		return nil, err
	}
	id := IdentityFrom(authCtx)
	if id != nil && id.Role == identity.RoleProvider {
		return nil, status.Error(codes.PermissionDenied, "provider tokens cannot use the job API")
	}
	return id, nil
}

// actor returns an audit actor string for the caller.
func actor(id *Identity) string {
	if id == nil {
		return "owner-bootstrap"
	}
	return id.Email
}

func submitError(err error) error {
	switch {
	case errors.Is(err, jobs.ErrQuotaQueued), errors.Is(err, jobs.ErrQuotaRunning),
		errors.Is(err, jobs.ErrQuotaResources):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, jobs.ErrUnsupportedType):
		return status.Error(codes.Unimplemented, err.Error())
	default:
		return status.Error(codes.InvalidArgument, err.Error())
	}
}

func jobToProto(j jobs.Job) *computev1.Job {
	pb := &computev1.Job{
		JobId:          j.ID,
		Name:           j.Name,
		TenantId:       j.UserID,
		ContainerImage: j.Image,
		Requirements: &computev1.ResourceRequirements{
			CpuCores:    j.CPUCores,
			MemoryBytes: j.MemBytes,
		},
		ErrorMessage: j.Error,
	}
	switch j.State {
	case jobs.StateQueued:
		pb.State = computev1.JobState_JOB_STATE_QUEUED
	case jobs.StateScheduling:
		pb.State = computev1.JobState_JOB_STATE_SCHEDULING
	case jobs.StateRunning:
		pb.State = computev1.JobState_JOB_STATE_RUNNING
	case jobs.StateCompleted:
		pb.State = computev1.JobState_JOB_STATE_COMPLETED
	case jobs.StateFailed:
		pb.State = computev1.JobState_JOB_STATE_FAILED
	case jobs.StateCancelled:
		pb.State = computev1.JobState_JOB_STATE_CANCELLED
	}
	if !j.CreatedAt.IsZero() {
		pb.CreatedAtNs = j.CreatedAt.UnixNano()
	}
	if !j.StartedAt.IsZero() {
		pb.StartedAtNs = j.StartedAt.UnixNano()
	}
	if !j.FinishedAt.IsZero() {
		pb.FinishedAtNs = j.FinishedAt.UnixNano()
	}
	return pb
}

// canAccess enforces ownership: consumers only touch their own jobs.
func (s *JobServer) canAccess(id *Identity, j jobs.Job) error {
	if id == nil || id.Role == identity.RoleOwner {
		return nil
	}
	if j.UserID != id.UserID {
		return status.Error(codes.PermissionDenied, "not your job")
	}
	return nil
}

func (s *JobServer) SubmitJob(ctx context.Context, req *computev1.SubmitJobRequest) (*computev1.SubmitJobResponse, error) {
	id, err := s.check(ctx)
	if err != nil {
		return nil, err
	}
	if req.Type != computev1.JobType_JOB_TYPE_UNKNOWN && req.Type != computev1.JobType_JOB_TYPE_SIMPLE {
		return nil, status.Errorf(codes.Unimplemented, "job type %s not supported yet (SIMPLE only)", req.Type)
	}
	userID := ""
	if id != nil {
		userID = id.UserID
	}
	cpu := float32(1)
	mem := uint64(1 << 30)
	if req.Requirements != nil {
		cpu = req.Requirements.CpuCores
		mem = req.Requirements.MemoryBytes
	}
	preq := jobs.SubmitRequest{
		Name:     req.Name,
		Image:    req.ContainerImage,
		Command:  req.Command,
		Env:      req.Env,
		CPUCores: cpu,
		MemBytes: mem,
	}
	if err := jobs.ValidateSubmit(preq, s.defs); err != nil {
		return nil, submitError(err)
	}
	if err := jobs.CheckSubmitQuotas(ctx, s.jobs, s.ids, userID, s.defs); err != nil {
		if s.aud != nil {
			s.aud.Append(ctx, audit.Entry{Actor: actor(id), Action: "job.submit",
				Object: req.Name, Detail: map[string]any{"image": req.ContainerImage}, Outcome: "denied"})
		}
		return nil, submitError(err)
	}
	j, err := s.jobs.Create(ctx, jobs.Job{
		UserID: userID, Name: req.Name, Image: req.ContainerImage,
		Command: req.Command, Env: req.Env, CPUCores: cpu, MemBytes: mem,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if s.aud != nil {
		s.aud.Append(ctx, audit.Entry{Actor: actor(id), Action: "job.submit",
			Object: j.ID, Detail: map[string]any{"name": j.Name, "image": j.Image}, Outcome: "ok"})
	}
	if s.sched != nil {
		s.sched.ScheduleOnce(ctx, 4)
	}
	log.Info().Str("job_id", j.ID).Str("user", actor(id)).Str("name", j.Name).Msg("job submitted")
	return &computev1.SubmitJobResponse{Accepted: true, JobId: j.ID}, nil
}

func (s *JobServer) GetJob(ctx context.Context, req *computev1.GetJobRequest) (*computev1.GetJobResponse, error) {
	id, err := s.check(ctx)
	if err != nil {
		return nil, err
	}
	j, err := s.jobs.Get(ctx, req.JobId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "job not found")
	}
	if err := s.canAccess(id, j); err != nil {
		return nil, err
	}
	return &computev1.GetJobResponse{Job: jobToProto(j)}, nil
}

func (s *JobServer) ListJobs(ctx context.Context, req *computev1.ListJobsRequest) (*computev1.ListJobsResponse, error) {
	id, err := s.check(ctx)
	if err != nil {
		return nil, err
	}
	limit := int(req.PageSize)
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var list []jobs.Job
	if id == nil || id.Role == identity.RoleOwner {
		list, err = s.jobs.ListAll(ctx, limit)
	} else {
		list, err = s.jobs.ListByUser(ctx, id.UserID, limit)
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &computev1.ListJobsResponse{}
	for _, j := range list {
		resp.Jobs = append(resp.Jobs, jobToProto(j))
	}
	return resp, nil
}

func (s *JobServer) CancelJob(ctx context.Context, req *computev1.CancelJobRequest) (*computev1.CancelJobResponse, error) {
	id, err := s.check(ctx)
	if err != nil {
		return nil, err
	}
	j, err := s.jobs.Get(ctx, req.JobId)
	if err != nil {
		return nil, status.Error(codes.NotFound, "job not found")
	}
	if err := s.canAccess(id, j); err != nil {
		return nil, err
	}
	if err := s.jobs.UpdateState(ctx, j.ID, jobs.StateCancelled, req.Reason); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "cannot cancel job in state %s", j.State)
	}
	// If a task was dispatched, ask the node to kill it on next heartbeat.
	if j.TaskID != "" && j.NodeID != "" && s.hub != nil {
		s.hub.EnqueueCancel(j.NodeID, j.TaskID)
	}
	if s.aud != nil {
		s.aud.Append(ctx, audit.Entry{Actor: actor(id), Action: "job.cancel",
			Object: j.ID, Detail: map[string]any{"reason": req.Reason}, Outcome: "ok"})
	}
	log.Info().Str("job_id", j.ID).Str("actor", actor(id)).Msg("job cancelled")
	return &computev1.CancelJobResponse{Success: true}, nil
}
