package identity

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Store is the persistence interface for the identity layer. PGStore
// (Postgres) is the production implementation; MemStore exists for unit
// tests. All methods are safe for concurrent use.
type Store interface {
	// Users.
	CreateUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, id string) (User, error)
	GetUserByEmail(ctx context.Context, email string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	SetUserStatus(ctx context.Context, id string, status UserStatus) error

	// Node tokens.
	IssueToken(ctx context.Context, userID, label string) (IssuedToken, error)
	GetTokenByHash(ctx context.Context, hash string) (NodeToken, error)
	ListTokens(ctx context.Context, userID string) ([]NodeToken, error)
	RevokeToken(ctx context.Context, tokenID string) error
	TouchToken(ctx context.Context, tokenID string, at time.Time) error

	// Node bindings.
	BindNode(ctx context.Context, nodeID, userID, tokenID string) error
	GetNodeBinding(ctx context.Context, nodeID string) (NodeBinding, error)
	UnbindNode(ctx context.Context, nodeID string) error

	// Invites.
	CreateInvite(ctx context.Context, inv Invite) error
	GetInvite(ctx context.Context, codeHash string) (Invite, error)
	ConsumeInvite(ctx context.Context, codeHash string) error
	ListInvites(ctx context.Context) ([]Invite, error)
	RevokeInvite(ctx context.Context, codeHash string) error

	// Access requests (web join flow).
	CreateAccessRequest(ctx context.Context, r AccessRequest) error
	GetAccessRequest(ctx context.Context, id string) (AccessRequest, error)
	ListAccessRequests(ctx context.Context, status string) ([]AccessRequest, error)
	DecideAccessRequest(ctx context.Context, id, decidedBy string, approve bool) (AccessRequest, error)
}

// Compile-time assertion that MemStore satisfies Store (PGStore asserted in
// its own file).
var (
	_ Store = (*MemStore)(nil)
)

// MemStore is an in-memory Store implementation for unit tests.
type MemStore struct {
	mu       sync.RWMutex
	users    map[string]User          // by id
	byEmail  map[string]string        // email -> id
	tokens   map[string]NodeToken     // by id
	byHash   map[string]string        // token_hash -> token id
	bindings map[string]NodeBinding   // node_id -> binding
	invites  map[string]Invite        // code_hash -> invite
	requests map[string]AccessRequest // by id
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() *MemStore {
	return &MemStore{
		users:    map[string]User{},
		byEmail:  map[string]string{},
		tokens:   map[string]NodeToken{},
		byHash:   map[string]string{},
		bindings: map[string]NodeBinding{},
		invites:  map[string]Invite{},
		requests: map[string]AccessRequest{},
	}
}

func (m *MemStore) CreateUser(_ context.Context, u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[u.ID]; ok {
		return ErrAlreadyExists
	}
	if _, ok := m.byEmail[u.Email]; ok {
		return ErrAlreadyExists
	}
	if u.ID == "" {
		u.ID = uuid.NewString()
	}
	m.users[u.ID] = u
	m.byEmail[u.Email] = u.ID
	return nil
}

func (m *MemStore) GetUser(_ context.Context, id string) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (m *MemStore) GetUserByEmail(_ context.Context, email string) (User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byEmail[email]
	if !ok {
		return User{}, ErrNotFound
	}
	return m.users[id], nil
}

func (m *MemStore) ListUsers(_ context.Context) ([]User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]User, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, u)
	}
	return out, nil
}

func (m *MemStore) SetUserStatus(_ context.Context, id string, status UserStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return ErrNotFound
	}
	u.Status = status
	u.UpdatedAt = time.Now()
	m.users[id] = u
	return nil
}

func (m *MemStore) IssueToken(ctx context.Context, userID, label string) (IssuedToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[userID]; !ok {
		return IssuedToken{}, ErrNotFound
	}
	plaintext, hash, err := GenerateToken()
	if err != nil {
		return IssuedToken{}, err
	}
	nt := NodeToken{
		ID:        uuid.NewString(),
		UserID:    userID,
		Label:     label,
		Status:    "active",
		CreatedAt: time.Now(),
	}
	m.tokens[nt.ID] = nt
	m.byHash[hash] = nt.ID
	_ = ctx
	return IssuedToken{NodeToken: nt, Plaintext: plaintext}, nil
}

func (m *MemStore) GetTokenByHash(_ context.Context, hash string) (NodeToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byHash[hash]
	if !ok {
		return NodeToken{}, ErrNotFound
	}
	return m.tokens[id], nil
}

func (m *MemStore) ListTokens(_ context.Context, userID string) ([]NodeToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []NodeToken
	for _, t := range m.tokens {
		if t.UserID == userID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *MemStore) RevokeToken(_ context.Context, tokenID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[tokenID]
	if !ok {
		return ErrNotFound
	}
	t.Status = "revoked"
	now := time.Now()
	t.RevokedAt = &now
	m.tokens[tokenID] = t
	return nil
}

func (m *MemStore) TouchToken(_ context.Context, tokenID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[tokenID]
	if !ok {
		return ErrNotFound
	}
	t.LastUsedAt = &at
	m.tokens[tokenID] = t
	return nil
}

func (m *MemStore) BindNode(_ context.Context, nodeID, userID, tokenID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindings[nodeID] = NodeBinding{
		NodeID: nodeID, UserID: userID, TokenID: tokenID, BoundAt: time.Now(),
	}
	return nil
}

func (m *MemStore) GetNodeBinding(_ context.Context, nodeID string) (NodeBinding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.bindings[nodeID]
	if !ok {
		return NodeBinding{}, ErrNotFound
	}
	return b, nil
}

func (m *MemStore) UnbindNode(_ context.Context, nodeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.bindings, nodeID)
	return nil
}

func (m *MemStore) CreateInvite(_ context.Context, inv Invite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.invites[inv.CodeHash]; ok {
		return ErrAlreadyExists
	}
	m.invites[inv.CodeHash] = inv
	return nil
}

func (m *MemStore) GetInvite(_ context.Context, codeHash string) (Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inv, ok := m.invites[codeHash]
	if !ok {
		return Invite{}, ErrNotFound
	}
	return inv, nil
}

func (m *MemStore) ConsumeInvite(_ context.Context, codeHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[codeHash]
	if !ok {
		return ErrNotFound
	}
	if inv.Revoked || time.Now().After(inv.ExpiresAt) {
		return ErrInviteInvalid
	}
	if inv.Uses >= inv.MaxUses {
		return ErrInviteExhausted
	}
	inv.Uses++
	m.invites[codeHash] = inv
	return nil
}

func (m *MemStore) ListInvites(_ context.Context) ([]Invite, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Invite, 0, len(m.invites))
	for _, inv := range m.invites {
		out = append(out, inv)
	}
	return out, nil
}

func (m *MemStore) RevokeInvite(_ context.Context, codeHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invites[codeHash]
	if !ok {
		return ErrNotFound
	}
	inv.Revoked = true
	m.invites[codeHash] = inv
	return nil
}

func (m *MemStore) CreateAccessRequest(_ context.Context, r AccessRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	m.requests[r.ID] = r
	return nil
}

func (m *MemStore) GetAccessRequest(_ context.Context, id string) (AccessRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.requests[id]
	if !ok {
		return AccessRequest{}, ErrNotFound
	}
	return r, nil
}

func (m *MemStore) ListAccessRequests(_ context.Context, status string) ([]AccessRequest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []AccessRequest
	for _, r := range m.requests {
		if status == "" || r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *MemStore) DecideAccessRequest(_ context.Context, id, decidedBy string, approve bool) (AccessRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.requests[id]
	if !ok {
		return AccessRequest{}, ErrNotFound
	}
	if r.Status != "pending" {
		return r, errors.New("identity: request already decided")
	}
	r.Status = "denied"
	if approve {
		r.Status = "approved"
	}
	r.DecidedBy = decidedBy
	now := time.Now()
	r.DecidedAt = &now
	m.requests[id] = r
	return r, nil
}
