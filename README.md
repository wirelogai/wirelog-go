# wirelog-go

[WireLog](https://wirelog.ai) analytics client for Go. **Zero dependencies** — stdlib only.

> Headless alternative to PostHog, Amplitude, and Mixpanel — designed for agents instead of dashboards.

## Install

```bash
go get github.com/wirelogai/wirelog-go
```

## Quick Start

```go
package main

import (
	"context"
	"fmt"

	wirelog "github.com/wirelogai/wirelog-go"
)

func main() {
	client := wirelog.New(wirelog.Config{
		APIKey: "sk_your_secret_key",
	})
	defer client.Close() // flushes remaining events

	// Track events — non-blocking, batched automatically
	client.Track(wirelog.Event{
		EventType:       "signup",
		UserID:          "u_123",
		EventProperties: map[string]any{"plan": "free"},
	})

	// Query analytics (returns Markdown by default)
	result, err := client.Query(context.Background(), "signup | last 7d | count by day")
	if err != nil {
		panic(err)
	}
	fmt.Println(result)

	// Identify a user (bind device → user, set profile)
	_, err = client.Identify(context.Background(), wirelog.IdentifyParams{
		UserID:         "alice@acme.org",
		DeviceID:       "dev_abc",
		UserProperties: map[string]any{"plan": "pro"},
	})
	if err != nil {
		panic(err)
	}
}
```

## Design Principles

This client is designed to **never break your application**:

- **Non-blocking**: `Track()` enqueues events and returns immediately — it never makes HTTP calls on the caller's goroutine
- **Bounded memory**: Internal queue has a fixed capacity (default 10,000). When full, new events are dropped rather than causing OOM
- **Rate-limited by default**: A per-instance token bucket and rolling windows protect against hot-loop bugs (see [Rate limiting](#rate-limiting))
- **Graceful shutdown**: `Close()` flushes all remaining events before returning
- **Automatic batching**: Events are sent in batches (default 10 per batch, or every 2 seconds)
- **Retry with backoff**: Transient failures (429, 5xx) are retried up to 3 times with exponential backoff. Honours the server's `Retry-After` header on 429.
- **Panic recovery**: The background worker recovers from panics and reports them via `OnError`
- **No panics**: The client never panics — all errors are handled internally or returned

## Configuration

```go
client := wirelog.New(wirelog.Config{
    // Required: API key (pk_, sk_, or aat_).
    // Falls back to WIRELOG_API_KEY env var.
    APIKey: "sk_...",

    // API base URL. Default: https://api.wirelog.ai
    // Falls back to WIRELOG_HOST env var.
    Host: "https://api.wirelog.ai",

    // Max events per batch request. Default: 10, max: 2000.
    BatchSize: 10,

    // Max time between automatic flushes. Default: 2s.
    FlushInterval: 2 * time.Second,

    // Max events buffered in memory. Default: 10000.
    QueueSize: 10000,

    // HTTP request timeout. Default: 30s.
    HTTPTimeout: 30 * time.Second,

    // Error callback for background errors (dropped events, failed flushes).
    // Must be safe for concurrent use.
    OnError: func(err error) {
        log.Printf("wirelog: %v", err)
    },

    // Disable all tracking (Track becomes no-op). Useful for tests.
    Disabled: os.Getenv("ENV") == "test",

    // Per-instance rate limiter (defaults are conservative — see below).
    RateLimit: wirelog.RateLimitConfig{
        EventsPerSecond: 1,    // token bucket refill rate
        Burst:           10,   // token bucket capacity
        EventsPerMinute: 60,
        EventsPerHour:   1000,
        EventsPerDay:    10000,
        MaxEventBytes:   65536, // 64 KiB per event
    },
})
defer client.Close()
```

## Rate limiting

Every client instance enforces a layered rate limit so a hot loop or
runaway code path can't trash your application or our backend:

| Layer | Default | What it catches |
|---|---|---|
| Token bucket | 1 evt/s, burst 10 | Hot loops (`for { Track() }`) — they hit the wall in microseconds |
| Per-minute window | 60 | Sustained chatty bugs (e.g. tracking inside a 60fps render loop) |
| Per-hour window | 1,000 | Slow leaks (1/sec for hours) |
| Per-day window | 10,000 | Multi-day issues |
| Per-event payload | 64 KiB | Pathologically large event properties |

When a check rejects an event, it's silently dropped and the corresponding
counter in `RateLimitStats` is incremented. Use `client.RateLimitStats()`
to inspect counters. Callers can also subscribe to `OnError` to receive
`ErrRateLimited` and `ErrPayloadTooLarge` per drop (note: high-volume
floods will fire many callbacks; read `RateLimitStats()` instead for hot
paths).

```go
stats := client.RateLimitStats()
log.Printf("dropped: burst=%d minute=%d hour=%d day=%d size=%d",
    stats.DroppedBurst, stats.DroppedPerMinute, stats.DroppedPerHour,
    stats.DroppedPerDay, stats.DroppedPayloadSize)
```

To disable rate limiting entirely (e.g., for trusted batch import):

```go
RateLimit: wirelog.RateLimitConfig{Disabled: true}
```

To loosen a single window without disabling the rest, set it to a very
large value. Setting any single field to zero leaves it at the default.

## API

### `client.Track(event)`

Enqueue an event for async delivery. Never blocks, never returns an error. Auto-generates `insert_id` and `time` if not provided.

```go
client.Track(wirelog.Event{
    EventType:       "page_view",
    UserID:          "u_123",
    DeviceID:        "d_456",
    SessionID:       "s_789",
    EventProperties: map[string]any{"page": "/pricing"},
    UserProperties:  map[string]any{"plan": "pro"},
    Origin:          "server",
})
```

### `client.Flush(ctx)`

Block until all currently buffered events are sent. Respects context cancellation.

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
err := client.Flush(ctx)
```

### `client.Close()`

Flush remaining events and stop the background worker. Blocks until done (10s timeout). Idempotent.

### `client.Query(ctx, q, opts...)`

Run a pipe DSL query. Returns decoded JSON (`map[string]any`) or a string (Markdown/CSV).

```go
// Markdown (default)
result, _ := client.Query(ctx, "signup | last 7d | count by day")

// JSON
result, _ := client.Query(ctx, "signup | last 7d | count", wirelog.WithFormat("json"))

// Discover events and properties
result, _ := client.Query(ctx, "inspect * | last 30d", wirelog.WithFormat("json"))
```

### `client.Identify(ctx, params)`

Bind a device to a user and/or set profile properties.

```go
result, err := client.Identify(ctx, wirelog.IdentifyParams{
    UserID:   "alice@acme.org",
    DeviceID: "dev_123",
    UserPropertyOps: &wirelog.UserPropertyOps{
        Set:     map[string]any{"plan": "pro"},
        SetOnce: map[string]any{"signup_source": "organic"},
        Add:     map[string]float64{"login_count": 1},
        Unset:   []string{"legacy_flag"},
    },
})
```

## Error Handling

`Track()` never returns errors — it's fire-and-forget by design. Background errors are reported via the `OnError` callback:

```go
client := wirelog.New(wirelog.Config{
    APIKey: "sk_...",
    OnError: func(err error) {
        var apiErr *wirelog.APIError
        if errors.As(err, &apiErr) {
            log.Printf("wirelog API error %d: %s", apiErr.StatusCode, apiErr.Body)
        } else if errors.Is(err, wirelog.ErrQueueFull) {
            log.Print("wirelog: event dropped, queue full")
        } else if errors.Is(err, wirelog.ErrRateLimited) {
            log.Printf("wirelog: %v", err) // per-instance rate limiter
        } else if errors.Is(err, wirelog.ErrPayloadTooLarge) {
            log.Print("wirelog: event dropped, payload exceeded MaxEventBytes")
        } else {
            log.Printf("wirelog: %v", err)
        }
    },
})
```

`Query()` and `Identify()` are synchronous and return errors directly.

## Zero Dependencies

This library uses only the Go standard library (`net/http`, `encoding/json`, `crypto/rand`, `sync`, `context`). No third-party packages.

## Learn More

- [WireLog](https://wirelog.ai) — headless analytics for agents and LLMs
- [Why WireLog vs PostHog/Amplitude](https://docs.wirelog.ai/guides/vs-posthog/) — comparison guide
- [Query language docs](https://docs.wirelog.ai/query-language/overview/)
- [API reference](https://docs.wirelog.ai/reference/api/)
