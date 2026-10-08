// Package audit implements the append-only audit log for security-relevant
// events: joins, leaves, auth failures, token lifecycle, and admin actions.
//
// Every record is written to Postgres (audit_log table). When no database is
// configured (trusted-LAN deployments without the identity layer), a no-op
// Audit is returned so call sites stay unchanged; zerolog lines are still
// emitted either way.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/rs/zerolog/log"
)

// Entry is one audit record.
type Entry struct {
	At      time.Time      // record timestamp
	Actor   string         // user email, node id, or "system"
	Action  string         // "user.create", "auth.fail", "token.revoke", ...
	Object  string         // target of the action (email, node id, token id)
	Detail  map[string]any // structured context
	Outcome string         // "ok" | "denied" | "error"
}

// Audit appends entries to the audit_log table.
type Audit struct {
	db      *sql.DB // nil = no-op
	enabled bool
}

// New returns an Audit backed by db. Pass nil to get a no-op audit for
// trusted-LAN deployments without the identity layer.
func New(db *sql.DB) *Audit {
	return &Audit{db: db, enabled: db != nil}
}

// Append records an entry. Never returns an error: audit failures are logged
// loudly but must not break the request path.
func (a *Audit) Append(ctx context.Context, e Entry) {
	if e.Outcome == "" {
		e.Outcome = "ok"
	}
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		detail = []byte("{}")
	}
	log.Info().
		Str("actor", e.Actor).
		Str("action", e.Action).
		Str("object", e.Object).
		Str("outcome", e.Outcome).
		Msg("audit")

	if !a.enabled {
		return
	}
	// Best-effort with timeout; audit must not block request handling.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, execErr := a.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, object, detail, outcome)
		 VALUES (now(), $1, $2, $3, $4::jsonb, $5)`,
		e.Actor, e.Action, e.Object, string(detail), e.Outcome)
	if execErr != nil {
		log.Error().Err(execErr).Str("action", e.Action).
			Msg("audit: FAILED to persist audit entry")
	}
}

// Tail returns the most recent n entries (for nmonitctl audit tail).
func (a *Audit) Tail(ctx context.Context, n int) ([]Entry, error) {
	if !a.enabled {
		return nil, nil
	}
	rows, err := a.db.QueryContext(ctx,
		`SELECT ts, actor, action, object, detail, outcome
		 FROM audit_log ORDER BY ts DESC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var (
			e       Entry
			ts      time.Time
			detailB []byte
		)
		if err := rows.Scan(&ts, &e.Actor, &e.Action, &e.Object, &detailB, &e.Outcome); err != nil {
			return nil, err
		}
		e.At = ts
		_ = json.Unmarshal(detailB, &e.Detail)
		out = append(out, e)
	}
	return out, rows.Err()
}
