# wirelog-go

[WireLog](https://wirelog.ai) analytics client for Go. **Zero dependencies** — stdlib only.

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
- **Graceful shutdown**: `Close()` flushes all remaining events before returning
- **Automatic batching**: Events are sent in batches (default 10 per batch, or every 2 seconds)
- **Retry with backoff**: Transient failures (429, 5xx) are retried up to 3 times with exponential backoff
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
})
defer client.Close()
```

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
- [Query language docs](https://docs.wirelog.ai/query-language)
- [API reference](https://docs.wirelog.ai/reference/api)
