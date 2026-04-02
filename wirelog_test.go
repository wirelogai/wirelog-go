package wirelog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Test helpers ---

type mockServer struct {
	server       *httptest.Server
	mu           sync.Mutex
	requests     []mockRequest
	responseCode int
	responseBody string
}

type mockRequest struct {
	Path    string
	Method  string
	Headers http.Header
	Body    map[string]any
}

func newMockServer() *mockServer {
	m := &mockServer{
		responseCode: http.StatusOK,
		responseBody: `{"accepted": 1}`,
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)

		m.mu.Lock()
		m.requests = append(m.requests, mockRequest{
			Path:    r.URL.Path,
			Method:  r.Method,
			Headers: r.Header.Clone(),
			Body:    parsed,
		})
		code := m.responseCode
		respBody := m.responseBody
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(respBody))
	}))
	return m
}

func (m *mockServer) close() {
	m.server.Close()
}

func (m *mockServer) url() string {
	return m.server.URL
}

func (m *mockServer) setResponse(code int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responseCode = code
	m.responseBody = body
}

func (m *mockServer) getRequests() []mockRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]mockRequest, len(m.requests))
	copy(result, m.requests)
	return result
}

func (m *mockServer) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *mockServer) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = nil
}

func testClient(url string, opts ...func(*Config)) *Client {
	cfg := Config{
		APIKey:        "sk_test_key",
		Host:          url,
		BatchSize:     10,
		FlushInterval: 50 * time.Millisecond,
		QueueSize:     1000,
		HTTPTimeout:   5 * time.Second,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return New(cfg)
}

// --- Tests ---

func TestTrackEnqueuesAndFlushes(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())
	c.Track(Event{EventType: "signup", UserID: "u_123"})

	err := c.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	reqs := m.getRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}

	if reqs[0].Path != "/track" {
		t.Errorf("expected path /track, got %s", reqs[0].Path)
	}

	events, ok := reqs[0].Body["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("expected 1 event in batch, got %v", reqs[0].Body)
	}

	event := events[0].(map[string]any)
	if event["event_type"] != "signup" {
		t.Errorf("expected event_type signup, got %v", event["event_type"])
	}
	if event["user_id"] != "u_123" {
		t.Errorf("expected user_id u_123, got %v", event["user_id"])
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestTrackAutoGeneratesInsertIDAndTime(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())
	c.Track(Event{EventType: "test"})

	err := c.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	reqs := m.getRequests()
	events := reqs[0].Body["events"].([]any)
	event := events[0].(map[string]any)

	if event["insert_id"] == nil || event["insert_id"] == "" {
		t.Error("expected auto-generated insert_id")
	}
	if event["time"] == nil || event["time"] == "" {
		t.Error("expected auto-generated time")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestTrackSetsLibrary(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())
	c.Track(Event{EventType: "test"})

	err := c.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	reqs := m.getRequests()
	events := reqs[0].Body["events"].([]any)
	event := events[0].(map[string]any)

	if event["library"] != "wirelog-go/"+Version {
		t.Errorf("expected library wirelog-go/%s, got %v", Version, event["library"])
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestTrackPreservesUserProvidedFields(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())
	c.Track(Event{
		EventType: "custom",
		InsertID:  "my_id",
		Time:      "2025-01-01T00:00:00Z",
		Library:   "my-lib/1.0",
	})

	err := c.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	reqs := m.getRequests()
	events := reqs[0].Body["events"].([]any)
	event := events[0].(map[string]any)

	if event["insert_id"] != "my_id" {
		t.Errorf("expected insert_id my_id, got %v", event["insert_id"])
	}
	if event["time"] != "2025-01-01T00:00:00Z" {
		t.Errorf("expected time 2025-01-01T00:00:00Z, got %v", event["time"])
	}
	if event["library"] != "my-lib/1.0" {
		t.Errorf("expected library my-lib/1.0, got %v", event["library"])
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestBatchFlushOnSize(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url(), func(cfg *Config) {
		cfg.BatchSize = 3
		cfg.FlushInterval = 10 * time.Second // don't flush on timer
	})

	for i := range 3 {
		c.Track(Event{EventType: "test", UserID: "u_" + string(rune('0'+i))})
	}

	// Wait for batch to be sent.
	time.Sleep(100 * time.Millisecond)

	if m.requestCount() != 1 {
		t.Errorf("expected 1 batch request after hitting batch size, got %d", m.requestCount())
	}

	reqs := m.getRequests()
	events := reqs[0].Body["events"].([]any)
	if len(events) != 3 {
		t.Errorf("expected 3 events in batch, got %d", len(events))
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestBatchFlushOnInterval(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url(), func(cfg *Config) {
		cfg.BatchSize = 100 // won't hit batch size
		cfg.FlushInterval = 50 * time.Millisecond
	})

	c.Track(Event{EventType: "test"})

	// Wait for interval flush.
	time.Sleep(200 * time.Millisecond)

	if m.requestCount() < 1 {
		t.Error("expected at least 1 request after flush interval")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestCloseFlushesRemaining(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url(), func(cfg *Config) {
		cfg.BatchSize = 100           // won't hit batch size
		cfg.FlushInterval = time.Hour // won't hit interval
	})

	for range 5 {
		c.Track(Event{EventType: "test"})
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reqs := m.getRequests()
	total := 0
	for _, r := range reqs {
		events := r.Body["events"].([]any)
		total += len(events)
	}
	if total != 5 {
		t.Errorf("expected 5 events flushed on close, got %d", total)
	}
}

func TestTrackAfterCloseIsNoOp(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Track after close should not panic or send.
	c.Track(Event{EventType: "test"})
	time.Sleep(100 * time.Millisecond)

	if m.requestCount() != 0 {
		t.Error("expected no requests after close")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())
	if err := c.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestDisabledClientIsNoOp(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := New(Config{
		APIKey:   "sk_test",
		Host:     m.url(),
		Disabled: true,
	})

	c.Track(Event{EventType: "test"})
	time.Sleep(100 * time.Millisecond)

	if m.requestCount() != 0 {
		t.Error("expected no requests from disabled client")
	}

	err := c.Flush(context.Background())
	if err != nil {
		t.Errorf("flush on disabled client should return nil, got %v", err)
	}

	err = c.Close()
	if err != nil {
		t.Errorf("close on disabled client should return nil, got %v", err)
	}
}

func TestQueueFullDropsEvent(t *testing.T) {
	m := newMockServer()
	defer m.close()

	var dropped atomic.Int32

	c := testClient(m.url(), func(cfg *Config) {
		cfg.QueueSize = 2
		cfg.BatchSize = 100
		cfg.FlushInterval = time.Hour
		cfg.OnError = func(err error) {
			if err == ErrQueueFull {
				dropped.Add(1)
			}
		}
	})

	// Fill queue.
	for range 10 {
		c.Track(Event{EventType: "test"})
	}

	if dropped.Load() == 0 {
		t.Error("expected some events to be dropped")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestSendsAPIKeyHeader(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())
	c.Track(Event{EventType: "test"})

	err := c.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	reqs := m.getRequests()
	apiKey := reqs[0].Headers.Get("X-API-Key")
	if apiKey != "sk_test_key" {
		t.Errorf("expected X-API-Key sk_test_key, got %s", apiKey)
	}

	ua := reqs[0].Headers.Get("User-Agent")
	if ua != "wirelog-go/"+Version {
		t.Errorf("expected User-Agent wirelog-go/%s, got %s", Version, ua)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestQuerySendsRequest(t *testing.T) {
	m := newMockServer()
	defer m.close()

	m.setResponse(http.StatusOK, `{"rows": [{"count": 42}]}`)

	c := testClient(m.url())
	defer c.Close()

	result, err := c.Query(context.Background(), "* | last 7d | count")
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	reqs := m.getRequests()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(reqs))
	}
	if reqs[0].Path != "/query" {
		t.Errorf("expected path /query, got %s", reqs[0].Path)
	}
	if reqs[0].Body["q"] != "* | last 7d | count" {
		t.Errorf("expected q, got %v", reqs[0].Body["q"])
	}
	if reqs[0].Body["format"] != "llm" {
		t.Errorf("expected default format llm, got %v", reqs[0].Body["format"])
	}

	resultMap, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	rows, ok := resultMap["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("expected 1 row, got %v", resultMap["rows"])
	}
}

func TestQueryWithOptions(t *testing.T) {
	m := newMockServer()
	defer m.close()

	m.setResponse(http.StatusOK, `{"rows": []}`)

	c := testClient(m.url())
	defer c.Close()

	_, err := c.Query(context.Background(), "* | count",
		WithFormat("json"),
		WithLimit(50),
		WithOffset(10),
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	reqs := m.getRequests()
	if reqs[0].Body["format"] != "json" {
		t.Errorf("expected format json, got %v", reqs[0].Body["format"])
	}
	// json.Unmarshal decodes numbers as float64.
	if reqs[0].Body["limit"] != float64(50) {
		t.Errorf("expected limit 50, got %v", reqs[0].Body["limit"])
	}
	if reqs[0].Body["offset"] != float64(10) {
		t.Errorf("expected offset 10, got %v", reqs[0].Body["offset"])
	}
}

func TestIdentifySendsRequest(t *testing.T) {
	m := newMockServer()
	defer m.close()

	m.setResponse(http.StatusOK, `{"ok": true}`)

	c := testClient(m.url())
	defer c.Close()

	result, err := c.Identify(context.Background(), IdentifyParams{
		UserID:         "alice@acme.org",
		DeviceID:       "dev_123",
		UserProperties: map[string]any{"plan": "pro"},
	})
	if err != nil {
		t.Fatalf("identify: %v", err)
	}
	if !result.OK {
		t.Error("expected ok=true")
	}

	reqs := m.getRequests()
	if reqs[0].Path != "/identify" {
		t.Errorf("expected path /identify, got %s", reqs[0].Path)
	}
	if reqs[0].Body["user_id"] != "alice@acme.org" {
		t.Errorf("expected user_id alice@acme.org, got %v", reqs[0].Body["user_id"])
	}
}

func TestIdentifyWithPropertyOps(t *testing.T) {
	m := newMockServer()
	defer m.close()

	m.setResponse(http.StatusOK, `{"ok": true}`)

	c := testClient(m.url())
	defer c.Close()

	_, err := c.Identify(context.Background(), IdentifyParams{
		UserID: "bob@acme.org",
		UserPropertyOps: &UserPropertyOps{
			Set:     map[string]any{"plan": "enterprise"},
			SetOnce: map[string]any{"signup_source": "sales"},
			Add:     map[string]float64{"login_count": 1},
			Unset:   []string{"legacy_flag"},
		},
	})
	if err != nil {
		t.Fatalf("identify: %v", err)
	}

	reqs := m.getRequests()
	ops, ok := reqs[0].Body["user_property_ops"].(map[string]any)
	if !ok {
		t.Fatal("expected user_property_ops")
	}
	if set, ok := ops["$set"].(map[string]any); !ok || set["plan"] != "enterprise" {
		t.Errorf("expected $set.plan=enterprise, got %v", ops["$set"])
	}
}

func TestAPIErrorOnNon2xx(t *testing.T) {
	m := newMockServer()
	defer m.close()

	m.setResponse(http.StatusForbidden, `{"error": "invalid key"}`)

	c := testClient(m.url())
	defer c.Close()

	_, err := c.Query(context.Background(), "* | count")
	if err == nil {
		t.Fatal("expected error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", apiErr.StatusCode)
	}
}

func TestRetryOnTransientError(t *testing.T) {
	m := newMockServer()
	defer m.close()

	// First two requests return 500, then 200.
	var callCount atomic.Int32
	m.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)

		m.mu.Lock()
		m.requests = append(m.requests, mockRequest{
			Path:    r.URL.Path,
			Method:  r.Method,
			Headers: r.Header.Clone(),
			Body:    parsed,
		})
		m.mu.Unlock()

		count := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error": "server error"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accepted": 1}`))
	})

	c := testClient(m.url(), func(cfg *Config) {
		cfg.BatchSize = 1
	})

	c.Track(Event{EventType: "test"})

	err := c.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Wait for retries.
	time.Sleep(5 * time.Second)

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if callCount.Load() != 3 {
		t.Errorf("expected 3 requests (2 retries + 1 success), got %d", callCount.Load())
	}
}

func TestNoRetryOnPermanentError(t *testing.T) {
	m := newMockServer()
	defer m.close()

	m.setResponse(http.StatusBadRequest, `{"error": "bad request"}`)

	var errCount atomic.Int32
	c := testClient(m.url(), func(cfg *Config) {
		cfg.BatchSize = 1
		cfg.OnError = func(_ error) {
			errCount.Add(1)
		}
	})

	c.Track(Event{EventType: "test"})

	err := c.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Should only have 1 request (no retries for 400).
	if m.requestCount() != 1 {
		t.Errorf("expected 1 request (no retry on 400), got %d", m.requestCount())
	}

	if errCount.Load() != 1 {
		t.Errorf("expected 1 error callback, got %d", errCount.Load())
	}
}

func TestConcurrentTrack(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url(), func(cfg *Config) {
		cfg.BatchSize = 10
		cfg.QueueSize = 10000
	})

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for range 10 {
				c.Track(Event{EventType: "concurrent_test"})
			}
		}(i)
	}
	wg.Wait()

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reqs := m.getRequests()
	total := 0
	for _, r := range reqs {
		events, ok := r.Body["events"].([]any)
		if ok {
			total += len(events)
		}
	}
	if total != 1000 {
		t.Errorf("expected 1000 events, got %d", total)
	}
}

func TestFlushContextCancellation(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url(), func(cfg *Config) {
		cfg.FlushInterval = time.Hour
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	err := c.Flush(ctx)
	if err == nil {
		t.Error("expected context cancellation error")
	}

	if closeErr := c.Close(); closeErr != nil {
		t.Fatalf("close: %v", closeErr)
	}
}

func TestQueryContextCancellation(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Query(ctx, "* | count")
	if err == nil {
		t.Error("expected context cancellation error")
	}
}

func TestDefaultConfig(t *testing.T) {
	c := New(Config{APIKey: "sk_test", Disabled: true})
	defer c.Close()

	if c.cfg.Host != defaultHost {
		t.Errorf("expected default host %s, got %s", defaultHost, c.cfg.Host)
	}
	if c.cfg.BatchSize != defaultBatchSize {
		t.Errorf("expected default batch size %d, got %d", defaultBatchSize, c.cfg.BatchSize)
	}
	if c.cfg.FlushInterval != defaultFlushInterval {
		t.Errorf("expected default flush interval %v, got %v", defaultFlushInterval, c.cfg.FlushInterval)
	}
	if c.cfg.QueueSize != defaultQueueSize {
		t.Errorf("expected default queue size %d, got %d", defaultQueueSize, c.cfg.QueueSize)
	}
	if c.cfg.HTTPTimeout != defaultHTTPTimeout {
		t.Errorf("expected default HTTP timeout %v, got %v", defaultHTTPTimeout, c.cfg.HTTPTimeout)
	}
}

func TestBatchSizeCappedAtAPIMax(t *testing.T) {
	c := New(Config{APIKey: "sk_test", BatchSize: 5000, Disabled: true})
	defer c.Close()

	if c.cfg.BatchSize != maxBatchAPISize {
		t.Errorf("expected batch size capped at %d, got %d", maxBatchAPISize, c.cfg.BatchSize)
	}
}

func TestHostTrailingSlashStripped(t *testing.T) {
	c := New(Config{APIKey: "sk_test", Host: "https://api.wirelog.ai/", Disabled: true})
	defer c.Close()

	if c.cfg.Host != "https://api.wirelog.ai" {
		t.Errorf("expected trailing slash stripped, got %s", c.cfg.Host)
	}
}

func TestEventPropertiesAndUserProperties(t *testing.T) {
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())
	c.Track(Event{
		EventType: "purchase",
		UserID:    "u_1",
		EventProperties: map[string]any{
			"amount":  99.99,
			"product": "widget",
			"tags":    []string{"sale", "featured"},
		},
		UserProperties: map[string]any{
			"plan":  "pro",
			"email": "user@example.com",
		},
	})

	err := c.Flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}

	reqs := m.getRequests()
	events := reqs[0].Body["events"].([]any)
	event := events[0].(map[string]any)

	ep, ok := event["event_properties"].(map[string]any)
	if !ok {
		t.Fatal("expected event_properties")
	}
	if ep["product"] != "widget" {
		t.Errorf("expected product=widget, got %v", ep["product"])
	}

	up, ok := event["user_properties"].(map[string]any)
	if !ok {
		t.Fatal("expected user_properties")
	}
	if up["plan"] != "pro" {
		t.Errorf("expected plan=pro, got %v", up["plan"])
	}

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestTrackNeverPanics(t *testing.T) {
	// Track should never panic even with weird inputs.
	m := newMockServer()
	defer m.close()

	c := testClient(m.url())

	// Empty event.
	c.Track(Event{})

	// Event with all fields.
	co := true
	c.Track(Event{
		EventType:        "test",
		UserID:           "u",
		DeviceID:         "d",
		SessionID:        "s",
		EventProperties:  map[string]any{"k": "v"},
		UserProperties:   map[string]any{"k": "v"},
		InsertID:         "id",
		Origin:           "server",
		ClientOriginated: &co,
		Time:             "2025-01-01T00:00:00Z",
		Library:          "test",
	})

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestGenerateInsertID(t *testing.T) {
	id1 := generateInsertID()
	id2 := generateInsertID()

	if id1 == "" || id2 == "" {
		t.Error("expected non-empty insert IDs")
	}
	if id1 == id2 {
		t.Error("expected unique insert IDs")
	}
	if len(id1) != 32 {
		t.Errorf("expected 32-char hex ID, got %d chars: %s", len(id1), id1)
	}
}

