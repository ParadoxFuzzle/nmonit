package identity

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRateLimiterBurst(t *testing.T) {
	rl := NewRateLimiter(1, 3) // 1/s, burst 3
	for i := 0; i < 3; i++ {
		if !rl.Allow("k") {
			t.Fatalf("request %d within burst should pass", i+1)
		}
	}
	if rl.Allow("k") {
		t.Fatal("4th immediate request should be limited")
	}
	if !rl.Allow("other") {
		t.Fatal("different key should have its own bucket")
	}
}

func TestRateLimiterRefill(t *testing.T) {
	rl := NewRateLimiter(10, 1)
	rl.now = func() time.Time { return fakeNow.Add(200 * time.Millisecond) } // 2 tokens accrued
	fakeNow = fakeNow.Add(0)
	if !rl.Allow("x") {
		t.Fatal("first should pass")
	}
	fakeNow = fakeNow.Add(1 * time.Second)
	if !rl.Allow("x") {
		t.Fatal("after refill should pass")
	}
}

var fakeNow = time.Now()

func TestQuotaDefault(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	u := User{Email: "q@example.com", Role: RoleProvider, Status: StatusActive}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUserByEmail(ctx, u.Email)
	for i := 0; i < DefaultMaxNodesPerUser; i++ {
		if err := s.BindNode(ctx, string(rune('a'+i))+"-node", got.ID, "tok"); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckNodeQuotaWithStore(ctx, s, got.ID); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}
}
