package identity

import (
	"context"
	"sync"
	"time"
)

// RateLimiter is a per-key token bucket. Not durable: limits reset on
// control-plane restart, which is acceptable for abuse mitigation (the
// authoritative audit trail persists in Postgres).
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   int     // bucket capacity
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter returns a limiter allowing rate events/second with burst.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{
		buckets: map[string]*bucket{},
		rate:    rate,
		burst:   burst,
		now:     time.Now,
	}
}

// Allow consumes one token for key. It never blocks.
func (r *RateLimiter) Allow(key string) bool {
	return r.AllowN(key, 1)
}

// AllowN consumes n tokens for key.
func (r *RateLimiter) AllowN(key string, n int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(r.burst), last: now}
		r.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * r.rate
	if b.tokens > float64(r.burst) {
		b.tokens = float64(r.burst)
	}
	b.last = now
	if b.tokens < float64(n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}

// Quota limits are persistent (Postgres) and enforced at node registration.

const (
	// DefaultMaxNodesPerUser caps concurrent registered nodes per provider.
	DefaultMaxNodesPerUser = 3
)

// quota columns live in the users table via ALTER in schema/002_quotas.sql.
type Quotas struct {
	MaxNodes int
}

// GetUserQuota returns the node quota for a user, falling back to the default.
func (p *PGStore) GetUserQuota(ctx context.Context, userID string) (int, error) {
	var maxNodes int
	err := p.db.QueryRowContext(ctx,
		`SELECT COALESCE(max_nodes, $2) FROM users WHERE id = $1`,
		userID, DefaultMaxNodesPerUser).Scan(&maxNodes)
	if err != nil {
		return DefaultMaxNodesPerUser, err
	}
	return maxNodes, nil
}

// CountUserNodes returns how many nodes are currently bound to a user.
func (p *PGStore) CountUserNodes(ctx context.Context, userID string) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_bindings WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}

// CheckNodeQuota returns ErrQuotaExceeded when the user is at their limit.
func (p *PGStore) CheckNodeQuota(ctx context.Context, userID string) error {
	maxNodes, err := p.GetUserQuota(ctx, userID)
	if err != nil {
		return err
	}
	count, err := p.CountUserNodes(ctx, userID)
	if err != nil {
		return err
	}
	if count >= maxNodes {
		return ErrQuotaExceeded
	}
	return nil
}

// MemStore quota defaults (in-memory impl tracks bindings, uses same default).
func (m *MemStore) CheckNodeQuota(ctx context.Context, userID string) error {
	m.mu.RLock()
	count := 0
	for _, b := range m.bindings {
		if b.UserID == userID {
			count++
		}
	}
	m.mu.RUnlock()
	if count >= DefaultMaxNodesPerUser {
		return ErrQuotaExceeded
	}
	return nil
}

// CheckNodeQuotaWithStore is a helper usable from the auth path.
func CheckNodeQuotaWithStore(ctx context.Context, store Store, userID string) error {
	type quotaChecker interface {
		CheckNodeQuota(ctx context.Context, userID string) error
	}
	if qc, ok := store.(quotaChecker); ok {
		return qc.CheckNodeQuota(ctx, userID)
	}
	return nil
}

var _ = context.Background
