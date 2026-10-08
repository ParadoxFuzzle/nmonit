// Package identity implements the per-contributor identity layer: users,
// per-node tokens (stored hashed), invite codes, node bindings, and the
// access-request flow that powers the public onboarding UI.
//
// Two store implementations satisfy Store:
//   - PGStore: PostgreSQL-backed (production; DSN via --pg-dsn)
//   - MemStore: in-memory (unit tests; zero external deps)
//
// Token format: "nmt_" + 32 bytes base64url. Only sha256(token) is persisted,
// so a database leak does not leak usable credentials. Tokens are verified by
// hashing the presented bearer and looking up the digest.
//
// Roles:
//   - owner:    cluster operator (full admin)
//   - provider: contributes compute (registers nodes)
//   - consumer: submits jobs (Phase 2)
//
// The shared --agent-token / --api-key secrets remain supported for
// trusted-LAN bootstrap and are checked before the identity store.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// Role is the authorization role of a user.
type Role string

const (
	RoleOwner    Role = "owner"
	RoleProvider Role = "provider"
	RoleConsumer Role = "consumer"
)

// UserStatus is the lifecycle state of a user.
type UserStatus string

const (
	StatusActive   UserStatus = "active"
	StatusPending  UserStatus = "pending"
	StatusDisabled UserStatus = "disabled"
)

// Errors returned by Store implementations. Callers should use errors.Is.
var (
	ErrNotFound        = errors.New("identity: not found")
	ErrAlreadyExists   = errors.New("identity: already exists")
	ErrRevoked         = errors.New("identity: token revoked")
	ErrDisabled        = errors.New("identity: user disabled")
	ErrPending         = errors.New("identity: user pending approval")
	ErrInviteInvalid   = errors.New("identity: invite invalid or expired")
	ErrInviteExhausted = errors.New("identity: invite exhausted")
)

// User is an identity record (never carries secrets).
type User struct {
	ID          string
	Email       string
	DisplayName string
	Role        Role
	Status      UserStatus
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NodeToken is a per-contributor credential metadata record. The raw secret
// is shown exactly once at issue time and never stored.
type NodeToken struct {
	ID         string
	UserID     string
	Label      string
	Status     string // "active" | "revoked"
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// IssuedToken is a NodeToken plus the one-time plaintext secret.
type IssuedToken struct {
	NodeToken
	Plaintext string // "nmt_..." — shown once
}

// Invite is an onboarding invitation code (code stored hashed).
type Invite struct {
	CodeHash  string
	Role      Role
	CreatedBy string
	MaxUses   int
	Uses      int
	ExpiresAt time.Time
	Revoked   bool
	CreatedAt time.Time
}

// AccessRequest is a pending signup from the web join form.
type AccessRequest struct {
	ID          string
	Email       string
	DisplayName string
	Role        Role
	InviteCode  string // hashed; empty = open request
	Status      string // "pending" | "approved" | "denied"
	RequestedAt time.Time
	DecidedBy   string
	DecidedAt   *time.Time
}

// NodeBinding maps a registry node id to the user that runs it.
type NodeBinding struct {
	NodeID  string
	UserID  string
	TokenID string
	BoundAt time.Time
}

// GenerateToken returns a fresh "nmt_..." plaintext and its sha256 hex hash.
func GenerateToken() (plaintext string, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plaintext = "nmt_" + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the sha256 hex digest of a plaintext token.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// HashInvite returns the sha256 hex digest of an invite code.
func HashInvite(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// GenerateInviteCode returns a readable 12-char code prefixed "NM-", e.g.
// "NM-3F9K2A7QX1B4".
func GenerateInviteCode() (string, error) {
	buf := make([]byte, 9)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "NM-" + base64.RawURLEncoding.EncodeToString(buf)[:12], nil
}

// ConstantTimeEqual is a constant-time string comparison for secrets.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// OwnerBootstrapToken wraps the literal shared --agent-token / --api-key
// secret. The auth layer checks it before consulting the identity store so
// trusted-LAN deployments and the owner's own tooling keep working unchanged.
type OwnerBootstrapToken struct {
	Token string
}

// Matches reports whether the presented credential equals the bootstrap token.
func (o OwnerBootstrapToken) Matches(presented string) bool {
	if o.Token == "" {
		return false
	}
	return ConstantTimeEqual(o.Token, presented)
}
