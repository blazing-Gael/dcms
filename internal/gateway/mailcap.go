package gateway

import (
	"sync"
	"time"
)

// Account-email and per-account credential caps (issue #50). These sit on top of
// the per-IP auth tier and bound two things that tier can't: how much mail an
// instance sends (so a loop can't burn a provider's daily quota and take down every
// real reset), and how many credential guesses an account tolerates across codes
// and endpoints (so rotating IPs and fresh codes can't walk their way in).
//
// All state is in-memory. A process restart resets the counters, which the operator
// controls and an attacker cannot force; durable counters are a possible follow-up.

const (
	// Short per-recipient burst throttle shared by password reset and OTP request,
	// so the two endpoints share one account-email budget rather than one each.
	defaultAccountMailPerMinute = 4
	defaultAccountMailBurst     = 2
	// Per-recipient daily cap on account email; a mailbomb at one address degrades to
	// this many a day. 0 in config ⇒ this default; a negative config value ⇒ no cap.
	defaultAccountMailPerRecipientDay = 10

	// Per-account credential-failure budget across codes/endpoints. Past the
	// threshold in the window the account's OTP + password checks lock, with an
	// exponential backoff, so total guesses are bounded regardless of new codes.
	defaultCredFailuresPerWindow = 10
	defaultCredFailureWindow     = time.Hour
	defaultCredLockBase          = 15 * time.Minute
	defaultCredLockMax           = 24 * time.Hour
)

// accountMailLimiter caps outbound account email: a short per-recipient burst, a
// per-recipient daily cap, and an optional instance-wide daily ceiling.
type accountMailLimiter struct {
	burst *memoryLimiter // short-window per recipient

	mu           sync.Mutex
	day          string         // current UTC date, for the daily rollover
	perRecipient map[string]int // recipient → sends today
	instance     int            // total sends today
	recipientDay int            // per-recipient daily cap; 0 ⇒ unlimited
	instanceDay  int            // instance-wide daily cap; 0 ⇒ unlimited
	now          func() time.Time
}

// newAccountMailLimiter builds the limiter. recipientDay/instanceDay of 0 mean
// "no cap" for that dimension (the caller applies the recipient default first).
func newAccountMailLimiter(recipientDay, instanceDay int) *accountMailLimiter {
	if recipientDay < 0 {
		recipientDay = 0
	}
	if instanceDay < 0 {
		instanceDay = 0
	}
	return &accountMailLimiter{
		burst:        newMemoryLimiter(defaultAccountMailPerMinute, defaultAccountMailBurst),
		perRecipient: map[string]int{},
		recipientDay: recipientDay,
		instanceDay:  instanceDay,
		now:          time.Now,
	}
}

// allow reports whether an account email to recipient may be sent now, plus a short
// reason when not (for a WARN log — the reason never includes the address). Budget
// is consumed only when it returns true; a call blocked by a daily cap does not
// spend a burst token.
func (m *accountMailLimiter) allow(recipient string) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	today := m.now().UTC().Format("2006-01-02")
	if today != m.day {
		m.day = today
		m.perRecipient = map[string]int{}
		m.instance = 0
	}
	if m.instanceDay > 0 && m.instance >= m.instanceDay {
		return false, "instance daily mail ceiling reached"
	}
	if m.recipientDay > 0 && m.perRecipient[recipient] >= m.recipientDay {
		return false, "recipient daily mail cap reached"
	}
	if ok, _ := m.burst.Allow(recipient); !ok {
		return false, "recipient send rate exceeded"
	}
	m.perRecipient[recipient]++
	m.instance++
	return true, ""
}

// credFailureBudget bounds failed OTP/password attempts per account across codes
// and endpoints. The per-code OTP cap resets with each new code; this does not — it
// counts failures per account in a rolling window and, past the threshold, locks
// that account's credential checks with an exponential backoff.
type credFailureBudget struct {
	mu       sync.Mutex
	entries  map[string]*credFailEntry
	max      int
	window   time.Duration
	lockBase time.Duration
	lockMax  time.Duration
	now      func() time.Time
}

type credFailEntry struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
	strikes     int // consecutive lockouts, for exponential backoff
}

func newCredFailureBudget() *credFailureBudget {
	return &credFailureBudget{
		entries:  map[string]*credFailEntry{},
		max:      defaultCredFailuresPerWindow,
		window:   defaultCredFailureWindow,
		lockBase: defaultCredLockBase,
		lockMax:  defaultCredLockMax,
		now:      time.Now,
	}
}

// locked reports whether an account is currently locked and, if so, for how long.
func (b *credFailureBudget) locked(key string) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entries[key]
	if e == nil {
		return false, 0
	}
	if now := b.now(); e.lockedUntil.After(now) {
		return true, e.lockedUntil.Sub(now)
	}
	return false, 0
}

// fail records one failed attempt for an account, rolling the window and, at the
// threshold, locking with an exponential backoff (base, 2×, 4×, … capped).
func (b *credFailureBudget) fail(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	e := b.entries[key]
	if e == nil {
		e = &credFailEntry{windowStart: now}
		b.entries[key] = e
	}
	if now.Sub(e.windowStart) > b.window {
		e.count = 0
		e.windowStart = now
	}
	e.count++
	if e.count >= b.max {
		lock := b.lockBase << e.strikes
		if lock > b.lockMax || lock <= 0 { // <=0 guards the shift overflowing
			lock = b.lockMax
		}
		e.strikes++
		e.lockedUntil = now.Add(lock)
		e.count = 0
		e.windowStart = now
	}
}

// reset clears an account's failures after a successful authentication.
func (b *credFailureBudget) reset(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, key)
}

// allowAccountMail reports whether an account email may be sent to recipient now,
// consuming the shared budget when it may. A suppressed send is logged at WARN — the
// only signal an operator gets — WITHOUT the recipient address (never logged).
func (s *Server) allowAccountMail(recipient, kind string) bool {
	if s.accountMail == nil {
		return true
	}
	ok, reason := s.accountMail.allow(recipient)
	if !ok {
		s.logger.Warn("account email suppressed by rate cap", "kind", kind, "reason", reason)
	}
	return ok
}
