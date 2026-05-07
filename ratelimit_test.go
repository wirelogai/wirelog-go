package wirelog

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a deterministic clock for tests. Advance() moves time forward.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// epoch is a stable reference point used by the rate-limiter tests.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// --- windowCounter ---

func TestWindowCounterEmptyTotalIsZero(t *testing.T) {
	w := newWindowCounter(time.Minute, 6, 60)
	if got := w.total(epoch); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestWindowCounterAddsAndCounts(t *testing.T) {
	w := newWindowCounter(time.Minute, 6, 60)
	for range 5 {
		w.add(epoch)
	}
	if got := w.total(epoch); got != 5 {
		t.Errorf("expected 5, got %d", got)
	}
}

func TestWindowCounterEvictsOldBuckets(t *testing.T) {
	w := newWindowCounter(time.Minute, 6, 60) // 6 × 10s buckets

	// Fill the first bucket.
	w.add(epoch)
	w.add(epoch)

	// Move forward 65s — first bucket is now outside the 60s window.
	later := epoch.Add(65 * time.Second)
	w.add(later)

	if got := w.total(later); got != 1 {
		t.Errorf("expected 1 (old bucket evicted), got %d", got)
	}
}

func TestWindowCounterDistributesAcrossBuckets(t *testing.T) {
	w := newWindowCounter(time.Minute, 6, 60) // 6 × 10s buckets

	// Add events at 5s intervals across the window.
	for i := range 6 {
		w.add(epoch.Add(time.Duration(i*10) * time.Second))
	}

	// All 6 should still be within the 60s window when measured at t=55s.
	if got := w.total(epoch.Add(55 * time.Second)); got != 6 {
		t.Errorf("expected 6 events across 6 buckets, got %d", got)
	}
}

// --- rateLimiter token bucket (L1) ---

func TestRateLimiterAllowsBurstUpToCapacity(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1,
		Burst:           10,
		EventsPerMinute: 1000,
		EventsPerHour:   1_000_000,
		EventsPerDay:    1_000_000,
	}, clock.Now)

	for i := range 10 {
		if got := r.Allow(); got != DropNone {
			t.Errorf("event %d: expected DropNone, got %v", i, got)
		}
	}
	if got := r.Allow(); got != DropBurst {
		t.Errorf("event 11: expected DropBurst, got %v", got)
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1,
		Burst:           5,
		EventsPerMinute: 1000,
		EventsPerHour:   1_000_000,
		EventsPerDay:    1_000_000,
	}, clock.Now)

	// Drain the burst.
	for range 5 {
		_ = r.Allow()
	}
	if r.Allow() != DropBurst {
		t.Fatal("expected burst to be drained")
	}

	// Wait 3 seconds — should refill 3 tokens.
	clock.Advance(3 * time.Second)

	for i := range 3 {
		if got := r.Allow(); got != DropNone {
			t.Errorf("after refill, event %d: expected DropNone, got %v", i, got)
		}
	}
	if got := r.Allow(); got != DropBurst {
		t.Errorf("after refill, expected DropBurst on 4th, got %v", got)
	}
}

func TestRateLimiterRefillCapsAtBurst(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1,
		Burst:           3,
		EventsPerMinute: 1000,
		EventsPerHour:   1_000_000,
		EventsPerDay:    1_000_000,
	}, clock.Now)

	// Wait 100s without sending — tokens should cap at Burst=3, not 100.
	clock.Advance(100 * time.Second)

	for i := range 3 {
		if got := r.Allow(); got != DropNone {
			t.Errorf("event %d: expected DropNone, got %v", i, got)
		}
	}
	if got := r.Allow(); got != DropBurst {
		t.Errorf("expected DropBurst on 4th (refill should cap at burst), got %v", got)
	}
}

// --- rateLimiter sustained windows (L2) ---

func TestRateLimiterEnforcesPerMinute(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1000, // burst out of the way
		Burst:           1000,
		EventsPerMinute: 5,
		EventsPerHour:   1_000_000,
		EventsPerDay:    1_000_000,
	}, clock.Now)

	for i := range 5 {
		if got := r.Allow(); got != DropNone {
			t.Errorf("event %d: expected DropNone, got %v", i, got)
		}
	}
	if got := r.Allow(); got != DropPerMinute {
		t.Errorf("expected DropPerMinute, got %v", got)
	}
}

func TestRateLimiterPerMinuteRecoversAfterWindow(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1000,
		Burst:           1000,
		EventsPerMinute: 3,
		EventsPerHour:   1_000_000,
		EventsPerDay:    1_000_000,
	}, clock.Now)

	for range 3 {
		_ = r.Allow()
	}
	if r.Allow() != DropPerMinute {
		t.Fatal("expected per-minute to be hit")
	}

	// Advance past the window.
	clock.Advance(61 * time.Second)

	if got := r.Allow(); got != DropNone {
		t.Errorf("after window expiry, expected DropNone, got %v", got)
	}
}

func TestRateLimiterEnforcesPerHour(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1000,
		Burst:           1000,
		EventsPerMinute: 1000,
		EventsPerHour:   10,
		EventsPerDay:    1_000_000,
	}, clock.Now)

	for range 10 {
		if got := r.Allow(); got != DropNone {
			t.Errorf("expected DropNone, got %v", got)
		}
	}
	if got := r.Allow(); got != DropPerHour {
		t.Errorf("expected DropPerHour, got %v", got)
	}
}

func TestRateLimiterEnforcesPerDay(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1000,
		Burst:           1000,
		EventsPerMinute: 1000,
		EventsPerHour:   1000,
		EventsPerDay:    7,
	}, clock.Now)

	// Spread across hours so per-hour doesn't trigger first.
	for i := range 7 {
		clock.Advance(time.Hour)
		if got := r.Allow(); got != DropNone {
			t.Errorf("event %d: expected DropNone, got %v", i, got)
		}
	}
	if got := r.Allow(); got != DropPerDay {
		t.Errorf("expected DropPerDay, got %v", got)
	}
}

// --- rateLimiter sustained drip scenario ---

func TestRateLimiterCatchesSustainedOnePerSecondLeak(t *testing.T) {
	// Scenario: a hot loop that fires 1 event/sec sustained for 2 days.
	// The token bucket alone wouldn't catch this (1/sec is the refill rate),
	// but the per-minute and per-hour windows must.
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1,
		Burst:           10,
		EventsPerMinute: 60,
		EventsPerHour:   1000,
		EventsPerDay:    10000,
	}, clock.Now)

	allowed := 0
	dropped := 0
	for range 7200 { // 2 hours of attempted 1/sec.
		clock.Advance(time.Second)
		if r.Allow() == DropNone {
			allowed++
		} else {
			dropped++
		}
	}

	// At 1/sec sustained, we'd attempt 7200 events. The per-hour cap of
	// 1000 should kick in and drop ~6200 of them.
	if allowed > 2200 {
		t.Errorf("expected per-hour cap to throttle below 2200 allowed, got %d (dropped %d)", allowed, dropped)
	}
	if dropped < 5000 {
		t.Errorf("expected significant drops from sustained leak, got dropped=%d allowed=%d", dropped, allowed)
	}
}

// --- rateLimiter Disabled ---

func TestRateLimiterDisabledAllowsEverything(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{Disabled: true}, clock.Now)

	for i := range 100_000 {
		if got := r.Allow(); got != DropNone {
			t.Fatalf("disabled limiter dropped event %d: %v", i, got)
		}
	}
	if r.Stats().Total() != 0 {
		t.Errorf("disabled limiter should not record drops, got %+v", r.Stats())
	}
}

// --- rateLimiter Stats ---

func TestRateLimiterStatsCountsByReason(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1,
		Burst:           2,
		EventsPerMinute: 5,
		EventsPerHour:   1_000_000,
		EventsPerDay:    1_000_000,
	}, clock.Now)

	// 2 allowed, 3 burst-dropped.
	for range 5 {
		_ = r.Allow()
	}
	if got, want := r.Stats().DroppedBurst, uint64(3); got != want {
		t.Errorf("DroppedBurst: got %d want %d", got, want)
	}

	// Advance 5s to refill, drain again.
	clock.Advance(5 * time.Second)
	for range 3 {
		_ = r.Allow()
	}
	// We've now logged 5 successes; per-minute=5, so any further allow is per-minute drop.
	clock.Advance(5 * time.Second)
	for range 3 {
		_ = r.Allow()
	}
	if r.Stats().DroppedPerMinute == 0 {
		t.Errorf("expected per-minute drops, got %+v", r.Stats())
	}

	r.recordPayloadDrop()
	r.recordPayloadDrop()
	if got, want := r.Stats().DroppedPayloadSize, uint64(2); got != want {
		t.Errorf("DroppedPayloadSize: got %d want %d", got, want)
	}
}

// --- rateLimiter concurrency ---

func TestRateLimiterConcurrentAccessIsSafe(t *testing.T) {
	clock := newFakeClock(epoch)
	r := newRateLimiter(RateLimitConfig{
		EventsPerSecond: 1000,
		Burst:           1000,
		EventsPerMinute: 1_000_000,
		EventsPerHour:   1_000_000,
		EventsPerDay:    1_000_000,
	}, clock.Now)

	var wg sync.WaitGroup
	var allowed atomic.Int64
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if r.Allow() == DropNone {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	// 5000 attempts, burst of 1000, no clock advance. Exactly 1000 should pass.
	if got := allowed.Load(); got != 1000 {
		t.Errorf("expected 1000 allowed (= burst), got %d", got)
	}
}

// --- DropReason String ---

func TestDropReasonString(t *testing.T) {
	cases := []struct {
		r    DropReason
		want string
	}{
		{DropNone, "ok"},
		{DropBurst, "burst"},
		{DropPerMinute, "per_minute"},
		{DropPerHour, "per_hour"},
		{DropPerDay, "per_day"},
		{DropPayloadTooLarge, "payload_too_large"},
		{DropReason(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("DropReason(%d).String(): got %q want %q", c.r, got, c.want)
		}
	}
}

// --- parseRetryAfter ---

func TestParseRetryAfterSeconds(t *testing.T) {
	got := parseRetryAfter("5", epoch)
	if got != 5*time.Second {
		t.Errorf("expected 5s, got %v", got)
	}
}

func TestParseRetryAfterEmpty(t *testing.T) {
	if got := parseRetryAfter("", epoch); got != 0 {
		t.Errorf("expected 0, got %v", got)
	}
}

func TestParseRetryAfterNegative(t *testing.T) {
	if got := parseRetryAfter("-5", epoch); got != 0 {
		t.Errorf("expected 0 for negative value, got %v", got)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	future := epoch.Add(30 * time.Second)
	header := future.Format(http.TimeFormat)
	got := parseRetryAfter(header, epoch)
	// Allow 1s tolerance for header-format precision.
	if got < 29*time.Second || got > 31*time.Second {
		t.Errorf("expected ~30s, got %v", got)
	}
}

func TestParseRetryAfterPastHTTPDate(t *testing.T) {
	past := epoch.Add(-30 * time.Second)
	header := past.Format(http.TimeFormat)
	if got := parseRetryAfter(header, epoch); got != 0 {
		t.Errorf("expected 0 for past date, got %v", got)
	}
}

func TestParseRetryAfterGarbage(t *testing.T) {
	if got := parseRetryAfter("not a date", epoch); got != 0 {
		t.Errorf("expected 0 for unparseable, got %v", got)
	}
}

// --- Client integration: rate limiter wired into Track ---

func TestClientTrackHonoursBurstLimit(t *testing.T) {
	m := newMockServer()
	defer m.close()

	clock := newFakeClock(epoch)
	c := newWithClock(Config{
		APIKey:        "sk_test",
		Host:          m.url(),
		BatchSize:     100,
		FlushInterval: 50 * time.Millisecond,
		QueueSize:     1000,
		HTTPTimeout:   5 * time.Second,
		RateLimit: RateLimitConfig{
			EventsPerSecond: 1,
			Burst:           5,
			EventsPerMinute: 1000,
			EventsPerHour:   1_000_000,
			EventsPerDay:    1_000_000,
		},
	}, clock.Now)
	defer c.Close()

	// Fire 100 events in a tight loop. Only the first 5 (burst) should make it.
	for range 100 {
		c.Track(Event{EventType: "spam"})
	}

	stats := c.RateLimitStats()
	if stats.DroppedBurst != 95 {
		t.Errorf("expected 95 burst drops, got %d (full stats: %+v)", stats.DroppedBurst, stats)
	}
}

func TestClientTrackRejectsOversizedPayload(t *testing.T) {
	m := newMockServer()
	defer m.close()

	var sawTooLarge atomic.Int32
	c := newWithClock(Config{
		APIKey:        "sk_test",
		Host:          m.url(),
		BatchSize:     10,
		FlushInterval: 50 * time.Millisecond,
		QueueSize:     100,
		HTTPTimeout:   5 * time.Second,
		RateLimit: RateLimitConfig{
			EventsPerSecond: 1000,
			Burst:           1000,
			EventsPerMinute: 1_000_000,
			EventsPerHour:   1_000_000,
			EventsPerDay:    1_000_000,
			MaxEventBytes:   200, // small for test
		},
		OnError: func(err error) {
			if errors.Is(err, ErrPayloadTooLarge) {
				sawTooLarge.Add(1)
			}
		},
	}, nil)
	defer c.Close()

	// One small event passes.
	c.Track(Event{EventType: "small"})

	// One big event is rejected.
	big := strings.Repeat("x", 1000)
	c.Track(Event{
		EventType:       "big",
		EventProperties: map[string]any{"blob": big},
	})

	if sawTooLarge.Load() != 1 {
		t.Errorf("expected 1 ErrPayloadTooLarge callback, got %d", sawTooLarge.Load())
	}

	stats := c.RateLimitStats()
	if stats.DroppedPayloadSize != 1 {
		t.Errorf("expected 1 payload drop in stats, got %+v", stats)
	}
}

func TestClientTrackDisabledRateLimiterPassesAll(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := newWithClock(Config{
		APIKey:        "sk_test",
		Host:          m.url(),
		BatchSize:     100,
		FlushInterval: 50 * time.Millisecond,
		QueueSize:     2000,
		HTTPTimeout:   5 * time.Second,
		RateLimit:     RateLimitConfig{Disabled: true},
	}, nil)
	defer c.Close()

	for range 1000 {
		c.Track(Event{EventType: "test"})
	}

	stats := c.RateLimitStats()
	if stats.Total() != 0 {
		t.Errorf("expected no drops with disabled limiter, got %+v", stats)
	}
}

// --- Retry-After integration ---

func TestSendBatchHonoursRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	srv := newMockServer()
	defer srv.close()

	srv.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accepted":1}`))
	})

	c := testClient(srv.url(), func(cfg *Config) {
		cfg.BatchSize = 1
	})
	defer c.Close()

	start := time.Now()
	c.Track(Event{EventType: "test"})
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// Wait long enough to be sure the retry attempt completed.
	time.Sleep(2 * time.Second)
	elapsed := time.Since(start)

	if attempts.Load() < 2 {
		t.Errorf("expected at least 2 attempts, got %d", attempts.Load())
	}
	// Retry-After said 1s. Total elapsed should be at least 1s.
	if elapsed < 900*time.Millisecond {
		t.Errorf("expected to honour ~1s Retry-After, elapsed only %v", elapsed)
	}
}

func TestAPIErrorParsesRetryAfter(t *testing.T) {
	srv := newMockServer()
	defer srv.close()

	srv.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	})

	c := testClient(srv.url())
	defer c.Close()

	_, err := c.Query(context.Background(), "* | count")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.RetryAfter != 7*time.Second {
		t.Errorf("expected RetryAfter=7s, got %v", apiErr.RetryAfter)
	}
}

func TestClientIdentifyHonoursRateLimiter(t *testing.T) {
	srv := newMockServer()
	defer srv.close()
	srv.setResponse(http.StatusOK, `{"ok":true}`)

	c := newWithClock(Config{
		APIKey:        "sk_test",
		Host:          srv.url(),
		BatchSize:     10,
		FlushInterval: 50 * time.Millisecond,
		QueueSize:     100,
		HTTPTimeout:   5 * time.Second,
		RateLimit: RateLimitConfig{
			EventsPerSecond: 1,
			Burst:           3,
			EventsPerMinute: 1000,
			EventsPerHour:   1_000_000,
			EventsPerDay:    1_000_000,
		},
	}, nil)
	defer c.Close()

	for range 3 {
		_, err := c.Identify(context.Background(), IdentifyParams{UserID: "u"})
		if err != nil {
			t.Fatalf("expected first 3 identifies to succeed, got %v", err)
		}
	}
	_, err := c.Identify(context.Background(), IdentifyParams{UserID: "u"})
	if err == nil {
		t.Fatal("expected ErrRateLimited on 4th call")
	}
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
	if got := c.RateLimitStats().DroppedBurst; got != 1 {
		t.Errorf("expected 1 burst drop, got %d", got)
	}
}

// --- New defaults verification ---

func TestNewAppliesRateLimitDefaults(t *testing.T) {
	c := New(Config{APIKey: "sk_test", Disabled: true})
	defer c.Close()

	if c.limiter == nil {
		t.Fatal("expected limiter to be initialized")
	}
	if c.limiter.cfg.EventsPerSecond != defaultEventsPerSecond {
		t.Errorf("EventsPerSecond default: got %v want %v", c.limiter.cfg.EventsPerSecond, defaultEventsPerSecond)
	}
	if c.limiter.cfg.Burst != defaultBurst {
		t.Errorf("Burst default: got %v want %v", c.limiter.cfg.Burst, defaultBurst)
	}
	if c.limiter.cfg.EventsPerMinute != defaultEventsPerMinute {
		t.Errorf("EventsPerMinute default: got %v want %v", c.limiter.cfg.EventsPerMinute, defaultEventsPerMinute)
	}
	if c.limiter.cfg.EventsPerHour != defaultEventsPerHour {
		t.Errorf("EventsPerHour default: got %v want %v", c.limiter.cfg.EventsPerHour, defaultEventsPerHour)
	}
	if c.limiter.cfg.EventsPerDay != defaultEventsPerDay {
		t.Errorf("EventsPerDay default: got %v want %v", c.limiter.cfg.EventsPerDay, defaultEventsPerDay)
	}
	if c.limiter.cfg.MaxEventBytes != defaultMaxEventBytes {
		t.Errorf("MaxEventBytes default: got %v want %v", c.limiter.cfg.MaxEventBytes, defaultMaxEventBytes)
	}
}
