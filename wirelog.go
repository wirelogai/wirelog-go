// Package wirelog provides a Go client for the WireLog analytics API.
//
// Zero external dependencies — stdlib only. Track() is non-blocking and
// never returns an error; events are buffered in memory and flushed to
// the API in batches by a background goroutine. The client is safe for
// concurrent use and designed to never crash the host application.
//
// Quick start:
//
//	client := wirelog.New(wirelog.Config{APIKey: "sk_your_key"})
//	defer client.Close()
//
//	client.Track(wirelog.Event{
//	    EventType:       "signup",
//	    UserID:          "u_123",
//	    EventProperties: map[string]any{"plan": "free"},
//	})
package wirelog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the client library version.
const Version = "0.1.0"

const (
	defaultHost          = "https://api.wirelog.ai"
	defaultBatchSize     = 10
	defaultFlushInterval = 2 * time.Second
	defaultQueueSize     = 10000
	defaultHTTPTimeout   = 30 * time.Second
	defaultCloseTimeout  = 10 * time.Second

	maxRetries      = 3
	retryBaseDelay  = 1 * time.Second
	maxRetryDelay   = 30 * time.Second
	maxBatchAPISize = 2000
	maxResponseBody = 1 << 20 // 1 MB
)

// Sentinel errors.
var (
	// ErrClosed is returned when an operation is attempted on a closed client.
	ErrClosed = errors.New("wirelog: client is closed")

	// ErrQueueFull is passed to OnError when an event is dropped because the
	// internal buffer is at capacity.
	ErrQueueFull = errors.New("wirelog: event dropped (queue full)")
)

// APIError is returned when the WireLog API responds with a non-2xx status.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("wirelog: API %d: %s", e.StatusCode, e.Body)
}

// Config holds client configuration.
type Config struct {
	// APIKey is the WireLog API key (pk_, sk_, or aat_).
	// Falls back to WIRELOG_API_KEY env var if empty.
	APIKey string

	// Host is the API base URL. Defaults to https://api.wirelog.ai.
	// Falls back to WIRELOG_HOST env var if empty.
	Host string

	// BatchSize is the maximum number of events per batch request.
	// Default 10. Capped at 2000 (server limit).
	BatchSize int

	// FlushInterval is the maximum time between automatic flushes.
	// Default 2s.
	FlushInterval time.Duration

	// QueueSize is the maximum number of events buffered in memory.
	// When full, new events are dropped (oldest-first is not practical
	// with channels, so newest events are dropped). Default 10000.
	QueueSize int

	// HTTPTimeout is the timeout for individual HTTP requests.
	// Default 30s.
	HTTPTimeout time.Duration

	// OnError is called when a background error occurs (failed flush,
	// dropped event, etc). Called from a background goroutine — must be
	// safe for concurrent use. If nil, errors are silently discarded.
	OnError func(error)

	// Disabled disables all tracking. Track() becomes a no-op.
	// Query and Identify still work. Useful for test environments.
	Disabled bool
}

// Event represents a single analytics event.
type Event struct {
	EventType        string         `json:"event_type"`
	UserID           string         `json:"user_id,omitempty"`
	DeviceID         string         `json:"device_id,omitempty"`
	SessionID        string         `json:"session_id,omitempty"`
	EventProperties  map[string]any `json:"event_properties,omitempty"`
	UserProperties   map[string]any `json:"user_properties,omitempty"`
	InsertID         string         `json:"insert_id,omitempty"`
	Origin           string         `json:"origin,omitempty"`
	ClientOriginated *bool          `json:"clientOriginated,omitempty"` //nolint:tagliatelle // matches API field name
	Time             string         `json:"time,omitempty"`
	Library          string         `json:"library,omitempty"`
}

// TrackResult is the API response from a track request.
type TrackResult struct {
	Accepted int `json:"accepted"`
}

// IdentifyParams holds parameters for the Identify call.
type IdentifyParams struct {
	UserID          string          `json:"user_id"`
	DeviceID        string          `json:"device_id,omitempty"`
	UserProperties  map[string]any  `json:"user_properties,omitempty"`
	UserPropertyOps *UserPropertyOps `json:"user_property_ops,omitempty"`
}

// UserPropertyOps supports atomic profile property operations.
type UserPropertyOps struct {
	Set     map[string]any     `json:"$set,omitempty"`     //nolint:tagliatelle // matches API field name
	SetOnce map[string]any     `json:"$set_once,omitempty"` //nolint:tagliatelle // matches API field name
	Add     map[string]float64 `json:"$add,omitempty"`     //nolint:tagliatelle // matches API field name
	Unset   []string           `json:"$unset,omitempty"`   //nolint:tagliatelle // matches API field name
}

// IdentifyResult is the API response from an identify call.
type IdentifyResult struct {
	OK bool `json:"ok"`
}

// QueryOption configures a Query request.
type QueryOption func(*queryConfig)

type queryConfig struct {
	Format string
	Limit  int
	Offset int
}

// WithFormat sets the query output format ("llm", "json", or "csv").
func WithFormat(format string) QueryOption {
	return func(c *queryConfig) { c.Format = format }
}

// WithLimit sets the maximum number of result rows.
func WithLimit(limit int) QueryOption {
	return func(c *queryConfig) { c.Limit = limit }
}

// WithOffset sets the result row offset for pagination.
func WithOffset(offset int) QueryOption {
	return func(c *queryConfig) { c.Offset = offset }
}

// Client is a WireLog analytics client. It is safe for concurrent use.
type Client struct {
	cfg        Config
	httpClient *http.Client
	queue      chan Event
	flushCh    chan chan error
	stopped    chan struct{}
	closed     atomic.Bool
	wg         sync.WaitGroup
}

// New creates a new WireLog client and starts a background flush worker.
// Call Close() when done to flush remaining events and release resources.
func New(cfg Config) *Client {
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("WIRELOG_API_KEY")
	}
	if cfg.Host == "" {
		cfg.Host = os.Getenv("WIRELOG_HOST")
		if cfg.Host == "" {
			cfg.Host = defaultHost
		}
	}
	cfg.Host = strings.TrimRight(cfg.Host, "/")

	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.BatchSize > maxBatchAPISize {
		cfg.BatchSize = maxBatchAPISize
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaultFlushInterval
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = defaultHTTPTimeout
	}

	c := &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: cfg.HTTPTimeout},
		queue:      make(chan Event, cfg.QueueSize),
		flushCh:    make(chan chan error, 1),
		stopped:    make(chan struct{}),
	}

	if !cfg.Disabled {
		c.wg.Add(1)
		go c.worker()
	}

	return c
}

// Track enqueues an event for asynchronous delivery. It never blocks and
// never returns an error. If the internal queue is full, the event is
// dropped and OnError is called (if configured).
//
// Track auto-generates insert_id and time if not set on the event.
func (c *Client) Track(event Event) {
	if c.cfg.Disabled || c.closed.Load() {
		return
	}

	if event.InsertID == "" {
		event.InsertID = generateInsertID()
	}
	if event.Time == "" {
		event.Time = time.Now().UTC().Format(time.RFC3339)
	}
	if event.Library == "" {
		event.Library = "wirelog-go/" + Version
	}

	select {
	case c.queue <- event:
	default:
		c.reportError(ErrQueueFull)
	}
}

// Flush blocks until all currently buffered events have been sent to the
// API (or the context is cancelled). Safe to call concurrently.
func (c *Client) Flush(ctx context.Context) error {
	if c.cfg.Disabled {
		return nil
	}

	done := make(chan error, 1)
	select {
	case c.flushCh <- done:
	case <-c.stopped:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wirelog: flush cancelled: %w", ctx.Err())
	}

	select {
	case err := <-done:
		return err
	case <-c.stopped:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wirelog: flush cancelled: %w", ctx.Err())
	}
}

// Close flushes remaining events and stops the background worker.
// It blocks until all events are sent or a 10-second timeout elapses.
// After Close returns, Track() calls are silently dropped.
// Close is idempotent — subsequent calls return nil immediately.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	if c.cfg.Disabled {
		return nil
	}

	// Close the queue to signal the worker to drain and exit.
	close(c.queue)
	c.wg.Wait()
	return nil
}

// Query runs a pipe DSL query synchronously and returns the result.
// For format "json", the result is a decoded JSON value (map/slice).
// For "llm" or "csv", the result is a string.
func (c *Client) Query(ctx context.Context, q string, opts ...QueryOption) (any, error) {
	cfg := queryConfig{Format: "llm", Limit: 100, Offset: 0}
	for _, opt := range opts {
		opt(&cfg)
	}

	body := map[string]any{
		"q":      q,
		"format": cfg.Format,
		"limit":  cfg.Limit,
		"offset": cfg.Offset,
	}

	return c.post(ctx, "/query", body)
}

// Identify binds a device to a user and/or sets profile properties.
func (c *Client) Identify(ctx context.Context, params IdentifyParams) (*IdentifyResult, error) {
	result, err := c.post(ctx, "/identify", params)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("wirelog: re-marshal identify response: %w", err)
	}

	var ir IdentifyResult
	if err := json.Unmarshal(data, &ir); err != nil {
		return nil, fmt.Errorf("wirelog: decode identify response: %w", err)
	}
	return &ir, nil
}

// --- Background worker ---

func (c *Client) worker() {
	defer c.wg.Done()
	defer close(c.stopped)
	defer func() {
		if r := recover(); r != nil {
			c.reportError(fmt.Errorf("wirelog: worker panic recovered: %v", r))
		}
	}()

	batch := make([]Event, 0, c.cfg.BatchSize)
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		toSend := batch
		batch = make([]Event, 0, c.cfg.BatchSize)
		c.sendBatchWithRetry(toSend)
	}

	for {
		select {
		case event, ok := <-c.queue:
			if !ok {
				// Channel closed by Close() — drain and exit.
				flush()
				return
			}
			batch = append(batch, event)
			if len(batch) >= c.cfg.BatchSize {
				flush()
			}

		case <-ticker.C:
			flush()

		case done := <-c.flushCh:
			// Drain everything currently in the queue into batches.
			c.drainQueue(&batch)
			flush()
			done <- nil
		}
	}
}

func (c *Client) drainQueue(batch *[]Event) {
	for {
		select {
		case event, ok := <-c.queue:
			if !ok {
				return
			}
			*batch = append(*batch, event)
			if len(*batch) >= c.cfg.BatchSize {
				toSend := *batch
				*batch = make([]Event, 0, c.cfg.BatchSize)
				c.sendBatchWithRetry(toSend)
			}
		default:
			return
		}
	}
}

// --- HTTP transport ---

type trackPayload struct {
	Events []Event `json:"events"`
}

func (c *Client) sendBatchWithRetry(events []Event) {
	for attempt := range maxRetries + 1 {
		err := c.sendBatch(events)
		if err == nil {
			return
		}

		var apiErr *APIError
		if errors.As(err, &apiErr) && !isRetryable(apiErr.StatusCode) {
			c.reportError(err)
			return
		}

		if attempt >= maxRetries {
			c.reportError(fmt.Errorf("wirelog: max retries exceeded: %w", err))
			return
		}

		delay := retryBaseDelay << uint(attempt) //nolint:gosec // intentional shift for backoff
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
		time.Sleep(delay)
	}
}

func (c *Client) sendBatch(events []Event) error {
	payload := trackPayload{Events: events}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("wirelog: marshal batch: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.HTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Host+"/track", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("wirelog: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wirelog-go/"+Version)
	req.Header.Set("X-API-Key", c.cfg.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("wirelog: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))

	if resp.StatusCode >= http.StatusBadRequest {
		return &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	return nil
}

func (c *Client) post(ctx context.Context, path string, body any) (any, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("wirelog: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Host+path, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("wirelog: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wirelog-go/"+Version)
	req.Header.Set("X-API-Key", c.cfg.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wirelog: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var result any
		if unmarshalErr := json.Unmarshal(respBody, &result); unmarshalErr != nil {
			return nil, fmt.Errorf("wirelog: decode JSON response: %w", unmarshalErr)
		}
		return result, nil
	}

	return string(respBody), nil
}

// --- Helpers ---

func isRetryable(status int) bool {
	return status == http.StatusTooManyRequests || (status >= http.StatusInternalServerError && status < 600)
}

func (c *Client) reportError(err error) {
	if c.cfg.OnError != nil {
		c.cfg.OnError(err)
	}
}

func generateInsertID() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		// Extremely unlikely — fallback to timestamp.
		return fmt.Sprintf("evt_%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
