package restapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ParadoxFuzzle/control-plane/internal/audit"
	"github.com/ParadoxFuzzle/control-plane/internal/identity"
	"github.com/rs/zerolog/log"
)

// IdentityHandlers serves the identity/onboarding surface. Mount on:
//
//	/join           GET+POST  public (rate-limited upstream)
//	/api/v1/admin/* all      owner-only (X-API-Key == --api-key)
type IdentityHandlers struct {
	store identity.Store
	audit *audit.Audit
	// ownerKey is the shared --api-key; only holders may call admin routes.
	ownerKey string
	// publicRoles are the roles the join form may request.
	publicRoles map[identity.Role]bool
}

// NewIdentityHandlers constructs the identity routes.
func NewIdentityHandlers(store identity.Store, aud *audit.Audit, ownerKey string) *IdentityHandlers {
	return &IdentityHandlers{
		store:       store,
		audit:       aud,
		ownerKey:    strings.TrimSpace(ownerKey),
		publicRoles: map[identity.Role]bool{identity.RoleProvider: true, identity.RoleConsumer: true},
	}
}

func (h *IdentityHandlers) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *IdentityHandlers) requireOwner(w http.ResponseWriter, r *http.Request) bool {
	key := r.Header.Get("X-API-Key")
	if key == "" {
		key = r.URL.Query().Get("api_key")
	}
	key = strings.TrimSpace(key)
	owner := strings.TrimSpace(h.ownerKey)
	if owner == "" || key == "" || !identity.ConstantTimeEqual(key, owner) {
		log.Warn().Int("key_len", len(key)).Int("expected_len", len(h.ownerKey)).
			Msg("admin auth failed")
		h.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "owner credentials required"})
		return false
	}
	return true
}

func (h *IdentityHandlers) actorFrom(r *http.Request) string {
	// Owner actions come with the API key; attribute them to "owner".
	if r.Header.Get("X-API-Key") == h.ownerKey {
		return "owner"
	}
	return r.Header.Get("X-Acting-As")
}

// --- Public join flow ---

// JoinHandler serves GET /join (HTML form) and POST /join (submit).
func (h *IdentityHandlers) JoinHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(joinFormHTML))
	case http.MethodPost:
		h.handleJoinPost(w, r)
	default:
		h.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

type joinPayload struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	InviteCode  string `json:"invite_code"`
	Role        string `json:"role"`
}

func (h *IdentityHandlers) handleJoinPost(w http.ResponseWriter, r *http.Request) {
	var p joinPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		// HTML form fallback.
		if err := r.ParseForm(); err != nil {
			h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		p = joinPayload{
			Email:       r.FormValue("email"),
			DisplayName: r.FormValue("display_name"),
			InviteCode:  r.FormValue("invite_code"),
			Role:        r.FormValue("role"),
		}
	}
	p.Email = strings.TrimSpace(strings.ToLower(p.Email))
	p.InviteCode = strings.TrimSpace(p.InviteCode)
	p.Role = strings.TrimSpace(p.Role)

	role := identity.RoleProvider
	if p.Role == string(identity.RoleConsumer) {
		role = identity.RoleConsumer
	}
	if !h.publicRoles[role] {
		h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid role"})
		return
	}
	if p.Email == "" || !strings.Contains(p.Email, "@") {
		h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid email required"})
		return
	}

	var inviteHash string
	if p.InviteCode != "" {
		inviteHash = identity.HashInvite(p.InviteCode)
		inv, err := h.store.GetInvite(r.Context(), inviteHash)
		if err != nil || inv.Revoked || time.Now().After(inv.ExpiresAt) || inv.Uses >= inv.MaxUses {
			h.audit.Append(r.Context(), audit.Entry{Actor: p.Email, Action: "join.fail",
				Object: "invite", Detail: map[string]any{"reason": "invalid invite"}, Outcome: "denied"})
			h.writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid or expired invite code"})
			return
		}
		role = inv.Role
	}

	req := identity.AccessRequest{
		Email:       p.Email,
		DisplayName: p.DisplayName,
		Role:        role,
		InviteCode:  inviteHash,
		Status:      "pending",
	}
	if err := h.store.CreateAccessRequest(r.Context(), req); err != nil {
		log.Error().Err(err).Msg("join: create request failed")
		h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	h.audit.Append(r.Context(), audit.Entry{Actor: p.Email, Action: "join.request",
		Object: string(role), Detail: map[string]any{"invite": p.InviteCode != ""}})
	h.writeJSON(w, http.StatusOK, map[string]string{
		"status":  "pending",
		"message": "request received — the cluster operator will review it shortly",
	})
}

// --- Owner admin API ---

// AdminUsersHandler serves GET/POST /api/v1/admin/users.
func (h *IdentityHandlers) AdminUsersHandler(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwner(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		users, err := h.store.ListUsers(r.Context())
		if err != nil {
			h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		h.writeJSON(w, http.StatusOK, users)
	case http.MethodPost:
		var body struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
			Role        string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		role := identity.Role(body.Role)
		switch role {
		case identity.RoleOwner, identity.RoleProvider, identity.RoleConsumer:
		default:
			h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be owner|provider|consumer"})
			return
		}
		u := identity.User{
			Email:       strings.ToLower(strings.TrimSpace(body.Email)),
			DisplayName: body.DisplayName,
			Role:        role,
			Status:      identity.StatusActive,
		}
		if err := h.store.CreateUser(r.Context(), u); err != nil {
			h.writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		h.audit.Append(r.Context(), audit.Entry{Actor: h.actorFrom(r), Action: "user.create", Object: u.Email,
			Detail: map[string]any{"role": string(role)}})
		h.writeJSON(w, http.StatusCreated, u)
	default:
		h.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// AdminUserStatusHandler serves PATCH /api/v1/admin/users/{id}/status.
func (h *IdentityHandlers) AdminUserStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwner(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		h.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/users/")
	id = strings.TrimSuffix(id, "/status")
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	status := identity.UserStatus(body.Status)
	switch status {
	case identity.StatusActive, identity.StatusDisabled:
	default:
		h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be active|disabled"})
		return
	}
	if err := h.store.SetUserStatus(r.Context(), id, status); err != nil {
		h.writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	h.audit.Append(r.Context(), audit.Entry{Actor: h.actorFrom(r), Action: "user.status",
		Object: id, Detail: map[string]any{"status": string(status)}})
	h.writeJSON(w, http.StatusOK, map[string]string{"status": string(status)})
}

// AdminTokensHandler serves GET/POST /api/v1/admin/tokens?user=<email>.
func (h *IdentityHandlers) AdminTokensHandler(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwner(w, r) {
		return
	}
	userEmail := r.URL.Query().Get("user")
	u, err := h.store.GetUserByEmail(r.Context(), userEmail)
	if err != nil {
		h.writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		tokens, err := h.store.ListTokens(r.Context(), u.ID)
		if err != nil {
			h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		h.writeJSON(w, http.StatusOK, tokens)
	case http.MethodPost:
		var body struct {
			Label string `json:"label"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		issued, err := h.store.IssueToken(r.Context(), u.ID, body.Label)
		if err != nil {
			h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		h.audit.Append(r.Context(), audit.Entry{Actor: h.actorFrom(r), Action: "token.issue",
			Object: u.Email, Detail: map[string]any{"label": body.Label, "token_id": issued.ID}})
		// The plaintext is returned exactly once.
		h.writeJSON(w, http.StatusCreated, issued)
	default:
		h.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// AdminTokenRevokeHandler serves POST /api/v1/admin/tokens/{id}/revoke.
func (h *IdentityHandlers) AdminTokenRevokeHandler(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwner(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		h.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	tokenID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/tokens/"), "/revoke")
	if err := h.store.RevokeToken(r.Context(), tokenID); err != nil {
		h.writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	h.audit.Append(r.Context(), audit.Entry{Actor: h.actorFrom(r), Action: "token.revoke", Object: tokenID})
	h.writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// AdminInvitesHandler serves GET/POST /api/v1/admin/invites.
func (h *IdentityHandlers) AdminInvitesHandler(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwner(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		invites, err := h.store.ListInvites(r.Context())
		if err != nil {
			h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		h.writeJSON(w, http.StatusOK, invites)
	case http.MethodPost:
		var body struct {
			Role    string `json:"role"`
			MaxUses int    `json:"max_uses"`
			ExpiryH int    `json:"expiry_hours"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		role := identity.Role(body.Role)
		if role != identity.RoleProvider && role != identity.RoleConsumer {
			h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role must be provider|consumer"})
			return
		}
		if body.MaxUses <= 0 {
			body.MaxUses = 1
		}
		if body.ExpiryH <= 0 {
			body.ExpiryH = 72
		}
		code, err := identity.GenerateInviteCode()
		if err != nil {
			h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		inv := identity.Invite{
			CodeHash:  identity.HashInvite(code),
			Role:      role,
			MaxUses:   body.MaxUses,
			ExpiresAt: time.Now().Add(time.Duration(body.ExpiryH) * time.Hour),
		}
		if err := h.store.CreateInvite(r.Context(), inv); err != nil {
			h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		h.audit.Append(r.Context(), audit.Entry{Actor: h.actorFrom(r), Action: "invite.create",
			Detail: map[string]any{"role": string(role), "max_uses": body.MaxUses}})
		// The code is returned in plaintext exactly once.
		h.writeJSON(w, http.StatusCreated, map[string]any{
			"code":       code,
			"role":       string(role),
			"max_uses":   body.MaxUses,
			"expires_in": body.ExpiryH,
		})
	default:
		h.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// AdminRequestsHandler serves GET/POST /api/v1/admin/requests.
func (h *IdentityHandlers) AdminRequestsHandler(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwner(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		requests, err := h.store.ListAccessRequests(r.Context(), r.URL.Query().Get("status"))
		if err != nil {
			h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		h.writeJSON(w, http.StatusOK, requests)
	case http.MethodPost:
		var body struct {
			ID      string `json:"id"`
			Approve bool   `json:"approve"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		req, err := h.store.DecideAccessRequest(r.Context(), body.ID, "owner", body.Approve)
		if err != nil {
			h.writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		// On approval: create the user active and issue the first node token.
		resp := map[string]any{"status": req.Status}
		if body.Approve {
			u := identity.User{
				Email:       req.Email,
				DisplayName: req.DisplayName,
				Role:        req.Role,
				Status:      identity.StatusActive,
			}
			if err := h.store.CreateUser(r.Context(), u); err != nil && err != identity.ErrAlreadyExists {
				h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			created, _ := h.store.GetUserByEmail(r.Context(), req.Email)
			issued, err := h.store.IssueToken(r.Context(), created.ID, "onboarding")
			if err != nil {
				h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			resp["email"] = created.Email
			resp["node_token"] = issued.Plaintext
		}
		h.audit.Append(r.Context(), audit.Entry{Actor: h.actorFrom(r), Action: "join.decide",
			Object: req.Email, Detail: map[string]any{"approved": body.Approve}})
		h.writeJSON(w, http.StatusOK, resp)
	default:
		h.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// AdminAuditHandler serves GET /api/v1/admin/audit?n=50.
func (h *IdentityHandlers) AdminAuditHandler(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwner(w, r) {
		return
	}
	if h.audit == nil {
		h.writeJSON(w, http.StatusOK, []any{})
		return
	}
	n := 50
	if r.URL.Query().Get("n") != "" {
		if _, err := fmt.Sscanf(r.URL.Query().Get("n"), "%d", &n); err != nil || n <= 0 || n > 500 {
			n = 50
		}
	}
	entries, err := h.audit.Tail(r.Context(), n)
	if err != nil {
		h.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.writeJSON(w, http.StatusOK, entries)
}

const joinFormHTML = `<!doctype html>
<html><head><title>Join nmonit</title>
<style>body{font-family:system-ui;max-width:480px;margin:4rem auto;padding:0 1rem}
input,select,button{width:100%;padding:.6rem;margin:.4rem 0;box-sizing:border-box}
button{background:#2d6cdf;color:#fff;border:0;cursor:pointer}</style></head>
<body><h2>Join the nmonit compute pool</h2>
<p>Have an invite code? Enter it with your details. The operator reviews requests before your node token is issued.</p>
<form method="POST" action="/join">
<label>Email<br><input name="email" type="email" required></label><br>
<label>Display name<br><input name="display_name"></label><br>
<label>Role<br><select name="role"><option value="provider">Contribute compute</option><option value="consumer">Submit jobs</option></select></label><br>
<label>Invite code (optional)<br><input name="invite_code"></label><br>
<button type="submit">Request access</button>
</form></body></html>`
