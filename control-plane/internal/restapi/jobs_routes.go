package restapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/ParadoxFuzzle/control-plane/internal/audit"
	"github.com/ParadoxFuzzle/control-plane/internal/identity"
	"github.com/ParadoxFuzzle/control-plane/internal/jobs"
)

// JobsHandlers serves the Phase 2 consumer job API under /api/v1/jobs.
//
// Authentication (same nmt_ tokens as agents, role-aware):
//   - X-API-Key == owner key  -> owner (sees all jobs, bypasses quotas)
//   - Authorization: Bearer nmt_... with a consumer-role user -> own jobs,
//     quotas enforced
//   - provider-role tokens are rejected
type JobsHandlers struct {
	jobs  jobs.Store
	ids   identity.Store
	aud   *audit.Audit
	hub   *jobs.Hub
	sched *jobs.Scheduler
	defs  jobs.QuotaDefaults

	// ownerKey is the control plane's --api-key (TrimSpace'd at mount).
	ownerKey string
}

// NewJobsHandlers wires the REST job handlers. All deps except ownerKey may
// be nil where the handler tolerates it (hub/sched nil = no dispatch).
func NewJobsHandlers(js jobs.Store, ids identity.Store, aud *audit.Audit,
	hub *jobs.Hub, sched *jobs.Scheduler, defs jobs.QuotaDefaults, ownerKey string) *JobsHandlers {
	return &JobsHandlers{
		jobs: js, ids: ids, aud: aud, hub: hub, sched: sched,
		defs: defs, ownerKey: strings.TrimSpace(ownerKey),
	}
}

// sentinel auth errors mapped onto HTTP statuses by errStatus.
var (
	errUnauthorized = errors.New("unauthorized")
	errForbidden    = errors.New("forbidden")
)

func errStatus(err error) int {
	switch {
	case errors.Is(err, errForbidden):
		return http.StatusForbidden
	default:
		return http.StatusUnauthorized
	}
}

// caller is the resolved request identity.
type jobCaller struct {
	email   string // "" = owner via API key
	userID  string // "" = owner via API key
	role    identity.Role
	isOwner bool
}

func (h *JobsHandlers) resolve(r *http.Request) (jobCaller, error) {
	if key := r.Header.Get("X-API-Key"); h.ownerKey != "" &&
		identity.ConstantTimeEqual(strings.TrimSpace(key), h.ownerKey) {
		return jobCaller{role: identity.RoleOwner, isOwner: true}, nil
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return jobCaller{}, errUnauthorized
	}
	tok := strings.TrimPrefix(auth, "Bearer ")
	if h.ids == nil || !strings.HasPrefix(tok, "nmt_") {
		return jobCaller{}, errUnauthorized
	}
	nt, err := h.ids.GetTokenByHash(r.Context(), identity.HashToken(tok))
	if err != nil || nt.Status != "active" {
		return jobCaller{}, errUnauthorized
	}
	u, err := h.ids.GetUser(r.Context(), nt.UserID)
	if err != nil || u.Status != identity.StatusActive {
		return jobCaller{}, errUnauthorized
	}
	if u.Role == identity.RoleProvider {
		return jobCaller{}, errForbidden
	}
	return jobCaller{email: u.Email, userID: u.ID, role: u.Role}, nil
}

func (h *JobsHandlers) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *JobsHandlers) jobJSON(j jobs.Job) map[string]any {
	out := map[string]any{
		"id":         j.ID,
		"name":       j.Name,
		"image":      j.Image,
		"command":    j.Command,
		"env":        j.Env,
		"cpu_cores":  j.CPUCores,
		"mem_bytes":  j.MemBytes,
		"state":      string(j.State),
		"node_id":    j.NodeID,
		"task_id":    j.TaskID,
		"created_at": j.CreatedAt,
	}
	if !j.StartedAt.IsZero() {
		out["started_at"] = j.StartedAt
	}
	if !j.FinishedAt.IsZero() {
		out["finished_at"] = j.FinishedAt
	}
	if j.Error != "" {
		out["error"] = j.Error
	}
	return out
}

// JobsHandler serves POST /api/v1/jobs (submit) and GET /api/v1/jobs (list).
func (h *JobsHandlers) JobsHandler(w http.ResponseWriter, r *http.Request) {
	caller, err := h.resolve(r)
	if err != nil {
		w.WriteHeader(errStatus(err))
		return
	}
	switch r.Method {
	case http.MethodPost:
		h.submit(w, r, caller)
	case http.MethodGet:
		h.list(w, r, caller)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *JobsHandlers) submit(w http.ResponseWriter, r *http.Request, caller jobCaller) {
	var in struct {
		Name     string            `json:"name"`
		Image    string            `json:"image"`
		Command  []string          `json:"command"`
		Env      map[string]string `json:"env"`
		CPUCores float32           `json:"cpu_cores"`
		MemBytes uint64            `json:"mem_bytes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	preq := jobs.SubmitRequest{
		Name: in.Name, Image: in.Image, Command: in.Command, Env: in.Env,
		CPUCores: in.CPUCores, MemBytes: in.MemBytes,
	}
	if err := jobs.ValidateSubmit(preq, h.defs); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := jobs.CheckSubmitQuotas(r.Context(), h.jobs, h.ids, caller.userID, h.defs); err != nil {
		if errors.Is(err, jobs.ErrQuotaQueued) || errors.Is(err, jobs.ErrQuotaRunning) ||
			errors.Is(err, jobs.ErrQuotaResources) {
			if h.aud != nil {
				h.aud.Append(r.Context(), audit.Entry{Actor: h.actorOf(caller), Action: "job.submit",
					Object: in.Name, Outcome: "denied"})
			}
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		http.Error(w, "quota check failed", http.StatusInternalServerError)
		return
	}
	j, err := h.jobs.Create(r.Context(), jobs.Job{
		UserID: caller.userID, Name: in.Name, Image: in.Image,
		Command: in.Command, Env: in.Env, CPUCores: in.CPUCores, MemBytes: in.MemBytes,
	})
	if err != nil {
		log.Printf("jobs: create failed for %q: %v", in.Name, err)
		http.Error(w, "failed to create job", http.StatusInternalServerError)
		return
	}
	if h.aud != nil {
		h.aud.Append(r.Context(), audit.Entry{Actor: h.actorOf(caller), Action: "job.submit",
			Object: j.ID, Detail: map[string]any{"name": j.Name, "image": j.Image}, Outcome: "ok"})
	}
	if h.sched != nil {
		h.sched.ScheduleOnce(r.Context(), 4)
	}
	h.writeJSON(w, http.StatusCreated, map[string]any{
		"accepted": true, "job_id": j.ID, "state": string(jobs.StateQueued),
	})
}

func (h *JobsHandlers) list(w http.ResponseWriter, r *http.Request, caller jobCaller) {
	limit := 50
	var list []jobs.Job
	var err error
	if caller.isOwner {
		list, err = h.jobs.ListAll(r.Context(), limit)
	} else {
		list, err = h.jobs.ListByUser(r.Context(), caller.userID, limit)
	}
	if err != nil {
		log.Printf("jobs: list failed (owner=%v): %v", caller.isOwner, err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, j := range list {
		out = append(out, h.jobJSON(j))
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// JobHandler serves GET and DELETE /api/v1/jobs/{id}.
func (h *JobsHandlers) JobHandler(w http.ResponseWriter, r *http.Request) {
	caller, err := h.resolve(r)
	if err != nil {
		w.WriteHeader(errStatus(err))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/jobs/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "job id required", http.StatusNotFound)
		return
	}
	j, err := h.jobs.Get(r.Context(), id)
	if err != nil {
		if !errors.Is(err, jobs.ErrNotFound) {
			log.Printf("jobs: get %s failed: %v", id, err)
		}
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	if !caller.isOwner && j.UserID != caller.userID {
		http.Error(w, "not your job", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.writeJSON(w, http.StatusOK, h.jobJSON(j))
	case http.MethodDelete:
		if err := h.jobs.UpdateState(r.Context(), j.ID, jobs.StateCancelled, "cancelled by "+h.actorOf(caller)); err != nil {
			http.Error(w, "cannot cancel job in state "+string(j.State), http.StatusConflict)
			return
		}
		if j.TaskID != "" && j.NodeID != "" && h.hub != nil {
			h.hub.EnqueueCancel(j.NodeID, j.TaskID)
		}
		if h.aud != nil {
			h.aud.Append(r.Context(), audit.Entry{Actor: h.actorOf(caller), Action: "job.cancel",
				Object: j.ID, Outcome: "ok"})
		}
		h.writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *JobsHandlers) actorOf(c jobCaller) string {
	if c.isOwner {
		return "owner-apikey"
	}
	return c.email
}
