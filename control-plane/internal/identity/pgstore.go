package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "embed"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for postgres:// DSNs
)

//go:embed schema/001_identity.sql
var schemaSQL string

// PGStore is the PostgreSQL-backed Store. On construction it applies the
// embedded schema (idempotent CREATE TABLE IF NOT EXISTS), so a fresh
// database is ready to use.
type PGStore struct {
	db       *sql.DB
	mu       sync.Mutex // serializes read-modify-write ops (invite consume)
	once     sync.Once
	ready    bool
	readyErr error
}

// NewPGStore opens a connection pool using dsn (e.g.
// "postgres://user:pass@host:5432/db") and applies the schema.
func NewPGStore(dsn string) (*PGStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("identity: open postgres: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	p := &PGStore{db: db}
	if err := p.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("identity: migrate: %w", err)
	}
	p.ready = true
	return p, nil
}

// migrate applies the embedded schema once per process.
func (p *PGStore) migrate() error {
	p.once.Do(func() {
		if _, err := p.db.Exec(schemaSQL); err != nil {
			p.readyErr = err
			return
		}
	})
	return p.readyErr
}

// Close releases the connection pool.
func (p *PGStore) Close() error { return p.db.Close() }

func pgErrIsUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}

func pgErrNotFound(err error) bool {
	return err != nil && errors.Is(err, sql.ErrNoRows)
}

// --- Users ---

func (p *PGStore) CreateUser(ctx context.Context, u User) error {
	if u.ID == "" {
		u.ID = uuid.NewString()
	}
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, role, status, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,now(),now())`,
		u.ID, u.Email, u.DisplayName, string(u.Role), string(u.Status))
	if pgErrIsUnique(err) {
		return ErrAlreadyExists
	}
	return err
}

func (p *PGStore) GetUser(ctx context.Context, id string) (User, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT id, email, display_name, role, status, created_at, updated_at
		 FROM users WHERE id=$1`, id)
	return scanUser(row)
}

func (p *PGStore) GetUserByEmail(ctx context.Context, email string) (User, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT id, email, display_name, role, status, created_at, updated_at
		 FROM users WHERE email=$1`, email)
	return scanUser(row)
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var role, status string
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &role, &status,
		&u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Role = Role(role)
	u.Status = UserStatus(status)
	return u, nil
}

func (p *PGStore) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, email, display_name, role, status, created_at, updated_at
		 FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (p *PGStore) SetUserStatus(ctx context.Context, id string, status UserStatus) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE users SET status=$2, updated_at=now() WHERE id=$1`, id, string(status))
	if err != nil {
		return err
	}
	return requireAffected(res, ErrNotFound)
}

// --- Node tokens ---

func (p *PGStore) IssueToken(ctx context.Context, userID, label string) (IssuedToken, error) {
	if _, err := p.GetUser(ctx, userID); err != nil {
		return IssuedToken{}, err
	}
	plaintext, hash, err := GenerateToken()
	if err != nil {
		return IssuedToken{}, err
	}
	id := uuid.NewString()
	_, err = p.db.ExecContext(ctx,
		`INSERT INTO node_tokens (id, user_id, token_hash, label, status, created_at)
		 VALUES ($1,$2,$3,$4,'active',now())`,
		id, userID, hash, label)
	if err != nil {
		return IssuedToken{}, err
	}
	nt := NodeToken{ID: id, UserID: userID, Label: label, Status: "active",
		CreatedAt: time.Now()}
	return IssuedToken{NodeToken: nt, Plaintext: plaintext}, nil
}

const tokenCols = `id, user_id, label, status, created_at, last_used_at, revoked_at`

func scanToken(row interface{ Scan(...any) error }) (NodeToken, error) {
	var t NodeToken
	err := row.Scan(&t.ID, &t.UserID, &t.Label, &t.Status,
		&t.CreatedAt, &t.LastUsedAt, &t.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeToken{}, ErrNotFound
	}
	if err != nil {
		return NodeToken{}, err
	}
	return t, nil
}

func (p *PGStore) GetTokenByHash(ctx context.Context, hash string) (NodeToken, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT `+tokenCols+` FROM node_tokens WHERE token_hash=$1`, hash)
	return scanToken(row)
}

func (p *PGStore) ListTokens(ctx context.Context, userID string) ([]NodeToken, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT `+tokenCols+` FROM node_tokens WHERE user_id=$1 ORDER BY created_at`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (p *PGStore) RevokeToken(ctx context.Context, tokenID string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE node_tokens SET status='revoked', revoked_at=now()
		 WHERE id=$1 AND status='active'`, tokenID)
	if err != nil {
		return err
	}
	return requireAffected(res, ErrNotFound)
}

func (p *PGStore) TouchToken(ctx context.Context, tokenID string, at time.Time) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE node_tokens SET last_used_at=$2 WHERE id=$1`, tokenID, at)
	return err
}

// --- Node bindings ---

func (p *PGStore) BindNode(ctx context.Context, nodeID, userID, tokenID string) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO node_bindings (node_id, user_id, token_id, bound_at)
		 VALUES ($1,$2,$3,now())
		 ON CONFLICT (node_id) DO UPDATE
		   SET user_id=EXCLUDED.user_id, token_id=EXCLUDED.token_id, bound_at=now()`,
		nodeID, userID, tokenID)
	return err
}

func (p *PGStore) GetNodeBinding(ctx context.Context, nodeID string) (NodeBinding, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT node_id, user_id, COALESCE(token_id,''), bound_at
		 FROM node_bindings WHERE node_id=$1`, nodeID)
	var b NodeBinding
	err := row.Scan(&b.NodeID, &b.UserID, &b.TokenID, &b.BoundAt)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeBinding{}, ErrNotFound
	}
	if err != nil {
		return NodeBinding{}, err
	}
	return b, nil
}

func (p *PGStore) UnbindNode(ctx context.Context, nodeID string) error {
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM node_bindings WHERE node_id=$1`, nodeID)
	if err != nil {
		return err
	}
	return requireAffected(res, ErrNotFound)
}

// --- Invites ---

func (p *PGStore) CreateInvite(ctx context.Context, inv Invite) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO invite_codes (code, role, created_by, max_uses, uses, expires_at, revoked, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,now())
		 ON CONFLICT (code) DO UPDATE
		   SET role=EXCLUDED.role, max_uses=EXCLUDED.max_uses,
		       expires_at=EXCLUDED.expires_at, revoked=EXCLUDED.revoked`,
		inv.CodeHash, string(inv.Role), inv.CreatedBy,
		inv.MaxUses, inv.Uses, inv.ExpiresAt, inv.Revoked)
	if pgErrIsUnique(err) {
		return ErrAlreadyExists
	}
	return err
}

const inviteCols = `code, role, COALESCE(created_by::text,''), max_uses, uses, expires_at, revoked, created_at`

func scanInvite(row interface{ Scan(...any) error }) (Invite, error) {
	var inv Invite
	var role string
	err := row.Scan(&inv.CodeHash, &role, &inv.CreatedBy, &inv.MaxUses,
		&inv.Uses, &inv.ExpiresAt, &inv.Revoked, &inv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	if err != nil {
		return Invite{}, err
	}
	inv.Role = Role(role)
	return inv, nil
}

func (p *PGStore) GetInvite(ctx context.Context, codeHash string) (Invite, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT `+inviteCols+` FROM invite_codes WHERE code=$1`, codeHash)
	return scanInvite(row)
}

func (p *PGStore) ConsumeInvite(ctx context.Context, codeHash string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Atomic conditional update: only consumes if valid and uses remain.
	res, err := p.db.ExecContext(ctx,
		`UPDATE invite_codes SET uses = uses + 1
		 WHERE code=$1 AND revoked=FALSE AND uses < max_uses AND expires_at > now()`,
		codeHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInviteInvalid
	}
	return nil
}

func (p *PGStore) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT `+inviteCols+` FROM invite_codes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (p *PGStore) RevokeInvite(ctx context.Context, codeHash string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE invite_codes SET revoked=TRUE WHERE code=$1`, codeHash)
	if err != nil {
		return err
	}
	return requireAffected(res, ErrNotFound)
}

// --- Access requests ---

func (p *PGStore) CreateAccessRequest(ctx context.Context, r AccessRequest) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	var codeHash any
	if r.InviteCode != "" {
		codeHash = r.InviteCode
	}
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO access_requests (id, email, display_name, role, invite_code, status, requested_at)
		 VALUES ($1,$2,$3,$4,$5,'pending',now())`,
		r.ID, r.Email, r.DisplayName, string(r.Role), codeHash)
	if pgErrIsUnique(err) {
		return ErrAlreadyExists
	}
	return err
}

const requestCols = `id, email, display_name, role, status, requested_at, COALESCE(decided_by::text,''), decided_at`

func scanRequest(row interface{ Scan(...any) error }) (AccessRequest, error) {
	var r AccessRequest
	var role, status string
	err := row.Scan(&r.ID, &r.Email, &r.DisplayName, &role, &status,
		&r.RequestedAt, &r.DecidedBy, &r.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AccessRequest{}, ErrNotFound
	}
	if err != nil {
		return AccessRequest{}, err
	}
	r.Role = Role(role)
	r.Status = status
	return r, nil
}

func (p *PGStore) GetAccessRequest(ctx context.Context, id string) (AccessRequest, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT `+requestCols+` FROM access_requests WHERE id=$1`, id)
	return scanRequest(row)
}

func (p *PGStore) ListAccessRequests(ctx context.Context, status string) ([]AccessRequest, error) {
	query := `SELECT ` + requestCols + ` FROM access_requests`
	var args []any
	if status != "" {
		query += ` WHERE status=$1`
		args = append(args, status)
	}
	query += ` ORDER BY requested_at DESC`
	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *PGStore) DecideAccessRequest(ctx context.Context, id, decidedBy string, approve bool) (AccessRequest, error) {
	status := "denied"
	if approve {
		status = "approved"
	}
	res, err := p.db.ExecContext(ctx,
		`UPDATE access_requests SET status=$2, decided_by=(SELECT id FROM users WHERE email=$3), decided_at=now()
		 WHERE id=$1 AND status='pending'`, id, status, decidedBy)
	if err != nil {
		return AccessRequest{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return AccessRequest{}, ErrNotFound
	}
	return p.GetAccessRequest(ctx, id)
}

// --- helpers ---

func requireAffected(res sql.Result, notFound error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound
	}
	return nil
}

// Compile-time assertion that PGStore satisfies Store.
var (
	_ Store = (*PGStore)(nil)
)
