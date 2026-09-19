package gateway

import (
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestAccountMailLimiter_DailyCaps(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}
	m := newAccountMailLimiter(3, 5) // 3/recipient/day, 5/instance/day
	m.now = clk.now
	m.burst.now = clk.now

	// Advance a minute between sends so the short-window burst never interferes —
	// this test isolates the daily caps.
	send := func(r string) (bool, string) {
		ok, reason := m.allow(r)
		clk.add(time.Minute)
		return ok, reason
	}

	for i := 0; i < 3; i++ {
		if ok, _ := send("a@x.com"); !ok {
			t.Fatalf("send %d to a should be allowed", i)
		}
	}
	if ok, reason := send("a@x.com"); ok || reason != "recipient daily mail cap reached" {
		t.Fatalf("4th to a: ok=%v reason=%q, want recipient cap", ok, reason)
	}

	// Instance total is now 3; b can send 2 more before the instance ceiling (5).
	if ok, _ := send("b@x.com"); !ok {
		t.Fatal("b send 1 should be allowed")
	}
	if ok, _ := send("b@x.com"); !ok {
		t.Fatal("b send 2 should be allowed")
	}
	if ok, reason := send("b@x.com"); ok || reason != "instance daily mail ceiling reached" {
		t.Fatalf("b send 3: ok=%v reason=%q, want instance ceiling", ok, reason)
	}

	// The next UTC day resets both counters.
	clk.add(24 * time.Hour)
	if ok, _ := send("a@x.com"); !ok {
		t.Fatal("after day rollover, a should send again")
	}
}

func TestAccountMailLimiter_BurstThrottle(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}
	m := newAccountMailLimiter(100, 0) // high daily caps so the burst is what bites
	m.now = clk.now
	m.burst.now = clk.now

	// burst = 2 with no clock advance: the 3rd rapid send to one recipient is
	// throttled by the short window, not a daily cap.
	if ok, _ := m.allow("a@x.com"); !ok {
		t.Fatal("send 1")
	}
	if ok, _ := m.allow("a@x.com"); !ok {
		t.Fatal("send 2")
	}
	if ok, reason := m.allow("a@x.com"); ok || reason != "recipient send rate exceeded" {
		t.Fatalf("send 3: ok=%v reason=%q, want burst throttle", ok, reason)
	}
}

func TestAccountMailLimiter_Unlimited(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}
	m := newAccountMailLimiter(-1, 0) // negative recipient ⇒ unlimited, 0 instance ⇒ unlimited
	m.now = clk.now
	m.burst.now = clk.now
	// Advance a minute between sends so the burst always has a token; with both daily
	// dimensions off, a send is never denied by a daily cap.
	for i := 0; i < 50; i++ {
		if ok, reason := m.allow("z@x.com"); !ok {
			t.Fatalf("send %d denied with no daily caps: %q", i, reason)
		}
		clk.add(time.Minute)
	}
}

func TestAccountMailLimiter_RecipientMapBounded(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}
	m := newAccountMailLimiter(0, 0) // no daily caps: only the map-bound guard applies
	m.now = clk.now
	m.burst.now = clk.now
	m.maxRecipients = 3 // low bound for the test

	// Sending to many distinct recipients (advancing so the burst never bites) must
	// not grow the map without bound — it stays at or under the guard.
	for i := range 20 {
		if ok, reason := m.allow("addr" + string(rune('a'+i)) + "@x.com"); !ok {
			t.Fatalf("send %d denied unexpectedly: %q", i, reason)
		}
		if got := len(m.perRecipient); got > m.maxRecipients {
			t.Fatalf("recipient map grew to %d, want ≤ %d", got, m.maxRecipients)
		}
		clk.add(time.Minute)
	}
}

func TestCredFailureBudget_LocksAndBacksOff(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}
	b := newCredFailureBudget()
	b.now = clk.now // max 10, window 1h, base lock 15m

	for i := 0; i < 9; i++ {
		b.fail("u1")
		if locked, _ := b.locked("u1"); locked {
			t.Fatalf("locked after only %d failures", i+1)
		}
	}
	b.fail("u1") // 10th ⇒ lock
	locked, d := b.locked("u1")
	if !locked || d <= 0 || d > 15*time.Minute {
		t.Fatalf("after 10 failures: locked=%v dur=%v, want ~15m lock", locked, d)
	}

	// Still locked mid-window, unlocked after the backoff elapses.
	clk.add(14 * time.Minute)
	if locked, _ := b.locked("u1"); !locked {
		t.Fatal("should still be locked at 14m")
	}
	clk.add(2 * time.Minute) // past 15m
	if locked, _ := b.locked("u1"); locked {
		t.Fatal("should be unlocked past the backoff")
	}

	// Backoff escalates: the next 10 failures lock for ~30m (2× base).
	for i := 0; i < 10; i++ {
		b.fail("u1")
	}
	_, d2 := b.locked("u1")
	if d2 <= 15*time.Minute {
		t.Fatalf("second lock dur=%v, want > 15m (exponential backoff)", d2)
	}

	// A success clears the budget.
	b.reset("u1")
	if locked, _ := b.locked("u1"); locked {
		t.Fatal("reset should clear the lock")
	}
}

func TestCredFailureBudget_WindowRolls(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}
	b := newCredFailureBudget()
	b.now = clk.now

	// Failures spread wider than the window never accumulate to a lock.
	for i := 0; i < 20; i++ {
		b.fail("u2")
		clk.add(2 * b.window)
	}
	if locked, _ := b.locked("u2"); locked {
		t.Fatal("failures outside the window should not lock")
	}
}
