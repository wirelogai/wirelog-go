package wirelog

import (
	"sync"
	"sync/atomic"
	"time"
)

// Defaults applied when RateLimitConfig fields are zero. Setting
// RateLimitConfig.Disabled = true bypasses all checks regardless of these.
const (
	defaultEventsPerSecond = 200.0
	defaultBurst           = 2000
	defaultEventsPerMinute = 10000
	defaultEventsPerHour   = 500000
	defaultEventsPerDay    = 10000000
	defaultMaxEventBytes   = 65536 // 64 KiB
)

// DropReason identifies why an event was dropped by the rate limiter.
// Returned from rateLimiter.Allow and surfaced in RateLimitStats.
type DropReason int

const (
	// DropNone means the event was allowed.
	DropNone DropReason = iota
	// DropBurst means the per-second token bucket was exhausted.
	DropBurst
	// DropPerMinute means the per-minute sustained limit was hit.
	DropPerMinute
	// DropPerHour means the per-hour sustained limit was hit.
	DropPerHour
	// DropPerDay means the per-day sustained limit was hit.
	DropPerDay
	// DropPayloadTooLarge means the serialized event exceeded MaxEventBytes.
	DropPayloadTooLarge
)

// String returns a stable, machine-readable identifier for the drop reason.
func (d DropReason) String() string {
	switch d {
	case DropNone:
		return "ok"
	case DropBurst:
		return "burst"
	case DropPerMinute:
		return "per_minute"
	case DropPerHour:
		return "per_hour"
	case DropPerDay:
		return "per_day"
	case DropPayloadTooLarge:
		return "payload_too_large"
	default:
		return "unknown"
	}
}

// RateLimitConfig configures the per-client-instance rate limiter.
// All numeric fields default to safe values when zero. Use Disabled = true
// to turn rate limiting off entirely. Use a very large value to effectively
// disable an individual window without disabling the whole limiter.
type RateLimitConfig struct {
	// Disabled bypasses all rate limit checks (token bucket and windows).
	Disabled bool

	// EventsPerSecond is the token bucket refill rate. Default 200.
	EventsPerSecond float64

	// Burst is the token bucket capacity. Default 2000.
	Burst int

	// EventsPerMinute is the rolling 60-second cap. Default 10000.
	EventsPerMinute int

	// EventsPerHour is the rolling 60-minute cap. Default 500000.
	EventsPerHour int

	// EventsPerDay is the rolling 24-hour cap. Default 10000000.
	EventsPerDay int

	// MaxEventBytes is the per-event JSON size cap. Default 65536 (64 KiB).
	// Set to a negative value to disable the size check.
	MaxEventBytes int
}

// RateLimitStats reports cumulative drop counters since client creation.
// Counters are monotonic and safe to read concurrently.
type RateLimitStats struct {
	DroppedBurst       uint64
	DroppedPerMinute   uint64
	DroppedPerHour     uint64
	DroppedPerDay      uint64
	DroppedPayloadSize uint64
}

// Total returns the sum of all drop counters.
func (s RateLimitStats) Total() uint64 {
	return s.DroppedBurst + s.DroppedPerMinute + s.DroppedPerHour + s.DroppedPerDay + s.DroppedPayloadSize
}

// rateLimiter is a per-client-instance rate limiter combining a token
// bucket (burst protection) with three rolling windows (sustained protection).
// All methods are safe for concurrent use.
type rateLimiter struct {
	cfg RateLimitConfig
	now func() time.Time

	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
	minute     *windowCounter
	hour       *windowCounter
	day        *windowCounter

	droppedBurst       atomic.Uint64
	droppedPerMinute   atomic.Uint64
	droppedPerHour     atomic.Uint64
	droppedPerDay      atomic.Uint64
	droppedPayloadSize atomic.Uint64
}

// newRateLimiter constructs a rate limiter. If now is nil, time.Now is used.
// Zero-valued config fields are filled with defaults.
func newRateLimiter(cfg RateLimitConfig, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	if cfg.EventsPerSecond <= 0 {
		cfg.EventsPerSecond = defaultEventsPerSecond
	}
	if cfg.Burst <= 0 {
		cfg.Burst = defaultBurst
	}
	if cfg.EventsPerMinute <= 0 {
		cfg.EventsPerMinute = defaultEventsPerMinute
	}
	if cfg.EventsPerHour <= 0 {
		cfg.EventsPerHour = defaultEventsPerHour
	}
	if cfg.EventsPerDay <= 0 {
		cfg.EventsPerDay = defaultEventsPerDay
	}
	if cfg.MaxEventBytes == 0 {
		cfg.MaxEventBytes = defaultMaxEventBytes
	}

	t0 := now()
	return &rateLimiter{
		cfg:        cfg,
		now:        now,
		tokens:     float64(cfg.Burst),
		lastRefill: t0,
		minute:     newWindowCounter(time.Minute, 6, cfg.EventsPerMinute),
		hour:       newWindowCounter(time.Hour, 12, cfg.EventsPerHour),
		day:        newWindowCounter(24*time.Hour, 24, cfg.EventsPerDay),
	}
}

// Allow returns DropNone if the event should pass, or a DropReason if not.
// When the event passes, all internal counters are advanced.
// Disabled limiters always return DropNone without touching counters.
func (r *rateLimiter) Allow() DropReason {
	if r.cfg.Disabled {
		return DropNone
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()

	// Refill tokens based on elapsed time. Cap at burst.
	elapsed := now.Sub(r.lastRefill).Seconds()
	if elapsed > 0 {
		r.tokens += elapsed * r.cfg.EventsPerSecond
		if r.tokens > float64(r.cfg.Burst) {
			r.tokens = float64(r.cfg.Burst)
		}
		r.lastRefill = now
	}

	if r.tokens < 1 {
		r.droppedBurst.Add(1)
		return DropBurst
	}
	if r.minute.total(now) >= r.cfg.EventsPerMinute {
		r.droppedPerMinute.Add(1)
		return DropPerMinute
	}
	if r.hour.total(now) >= r.cfg.EventsPerHour {
		r.droppedPerHour.Add(1)
		return DropPerHour
	}
	if r.day.total(now) >= r.cfg.EventsPerDay {
		r.droppedPerDay.Add(1)
		return DropPerDay
	}

	// Consume.
	r.tokens--
	r.minute.add(now)
	r.hour.add(now)
	r.day.add(now)
	return DropNone
}

// RecordPayloadDrop increments the payload-too-large counter without
// consuming any token-bucket capacity. Called when L5 rejects an event
// after it already passed L1+L2.
func (r *rateLimiter) recordPayloadDrop() {
	r.droppedPayloadSize.Add(1)
}

// Stats returns a snapshot of cumulative drop counters.
func (r *rateLimiter) Stats() RateLimitStats {
	return RateLimitStats{
		DroppedBurst:       r.droppedBurst.Load(),
		DroppedPerMinute:   r.droppedPerMinute.Load(),
		DroppedPerHour:     r.droppedPerHour.Load(),
		DroppedPerDay:      r.droppedPerDay.Load(),
		DroppedPayloadSize: r.droppedPayloadSize.Load(),
	}
}

// MaxEventBytes returns the configured per-event size limit, or 0 if disabled.
func (r *rateLimiter) MaxEventBytes() int {
	if r.cfg.MaxEventBytes < 0 {
		return 0
	}
	return r.cfg.MaxEventBytes
}

// windowCounter is a bucketed sliding-window counter. It approximates a
// true sliding window using numBuckets fixed-size sub-windows. Memory is
// O(numBuckets); add/total are also O(numBuckets) but numBuckets is small
// (6, 12, or 24 in practice).
type windowCounter struct {
	bucketDuration time.Duration
	numBuckets     int
	buckets        []windowBucket
}

type windowBucket struct {
	start time.Time
	count int
}

// newWindowCounter creates a counter that tracks events over `window`
// using `numBuckets` sub-buckets. The unused `limit` parameter is intentional —
// the limit is enforced by the caller (rateLimiter.Allow) so the counter
// can be inspected and tested independently.
func newWindowCounter(window time.Duration, numBuckets, _ int) *windowCounter {
	return &windowCounter{
		bucketDuration: window / time.Duration(numBuckets),
		numBuckets:     numBuckets,
		buckets:        make([]windowBucket, numBuckets),
	}
}

// total returns the number of events recorded within the window ending at t.
func (w *windowCounter) total(t time.Time) int {
	cutoff := t.Add(-w.bucketDuration * time.Duration(w.numBuckets))
	sum := 0
	for _, b := range w.buckets {
		if b.count == 0 {
			continue
		}
		if b.start.After(cutoff) || b.start.Equal(cutoff) {
			sum += b.count
		}
	}
	return sum
}

// add records one event at time t into the appropriate bucket.
// If t falls in a bucket older than any tracked, the oldest is evicted.
func (w *windowCounter) add(t time.Time) {
	bucketStart := t.Truncate(w.bucketDuration)
	for i := range w.buckets {
		if w.buckets[i].start.Equal(bucketStart) {
			w.buckets[i].count++
			return
		}
	}
	oldestIdx := 0
	for i := range w.buckets {
		if w.buckets[i].start.Before(w.buckets[oldestIdx].start) {
			oldestIdx = i
		}
	}
	w.buckets[oldestIdx] = windowBucket{start: bucketStart, count: 1}
}
