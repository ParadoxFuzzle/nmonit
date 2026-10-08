package jobs

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ParadoxFuzzle/control-plane/internal/identity"
)

// SubmitRequest is the transport-neutral job submission payload (REST JSON
// and gRPC SubmitJobRequest both map into it).
type SubmitRequest struct {
	Name     string
	Image    string
	Command  []string
	Env      map[string]string
	CPUCores float32
	MemBytes uint64
}

var jobNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// ValidateSubmit checks payload shape and per-job resource caps. The
// returned errors are human-readable; transport layers map them onto 4xx /
// codes.InvalidArgument.
func ValidateSubmit(req SubmitRequest, defs QuotaDefaults) error {
	if strings.TrimSpace(req.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if !jobNameRe.MatchString(req.Name) {
		return fmt.Errorf("name must match %s (letters, digits, ., _, -)", jobNameRe.String())
	}
	if strings.TrimSpace(req.Image) == "" {
		return fmt.Errorf("image is required")
	}
	if req.CPUCores <= 0 || req.CPUCores > defs.MaxJobCores {
		return fmt.Errorf("%w: cpu_cores must be > 0 and <= %.1f", ErrQuotaResources, defs.MaxJobCores)
	}
	if req.MemBytes == 0 || req.MemBytes > defs.MaxJobMemBytes {
		return fmt.Errorf("%w: mem_bytes must be > 0 and <= %d", ErrQuotaResources, defs.MaxJobMemBytes)
	}
	return nil
}

// effectiveQuota resolves per-user overrides (users.max_jobs_*) against the
// control-plane defaults. nil user = defaults.
func effectiveQuota(u *identity.User, defs QuotaDefaults) (maxRunning, maxQueued int) {
	maxRunning, maxQueued = defs.MaxRunning, defs.MaxQueued
	if u == nil {
		return
	}
	if u.MaxJobsRunning != nil && *u.MaxJobsRunning >= 0 {
		maxRunning = *u.MaxJobsRunning
	}
	if u.MaxJobsQueued != nil && *u.MaxJobsQueued >= 0 {
		maxQueued = *u.MaxJobsQueued
	}
	return
}

// CheckSubmitQuotas enforces the per-user queued and running job quotas.
// userID may be "" for owner/bootstrap submissions, which bypass quotas.
func CheckSubmitQuotas(ctx context.Context, s Store, ids identity.Store, userID string, defs QuotaDefaults) error {
	if userID == "" {
		return nil // owner path: no quotas
	}
	var u *identity.User
	if ids != nil {
		if user, err := ids.GetUser(ctx, userID); err == nil {
			u = &user
		}
	}
	maxRunning, maxQueued := effectiveQuota(u, defs)

	queued, err := s.CountByState(ctx, userID, StateQueued)
	if err != nil {
		return err
	}
	if queued >= maxQueued {
		return fmt.Errorf("%w: %d of %d queued slots used", ErrQuotaQueued, queued, maxQueued)
	}

	active := 0
	for _, st := range ActiveStates {
		n, err := s.CountByState(ctx, userID, st)
		if err != nil {
			return err
		}
		active += n
	}
	if active >= maxRunning {
		return fmt.Errorf("%w: %d running/scheduling jobs (limit %d)", ErrQuotaRunning, active, maxRunning)
	}
	return nil
}
