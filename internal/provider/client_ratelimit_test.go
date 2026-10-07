package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflogtest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Tests for the 429 retry in nxipClient.do (item 1 of the Weft Networks
// feedback, docs/research/weft-feedback-triage-2026-10-07.md in
// net-saas-monorepo). The API's 429 carries Retry-After and a sentence in
// `message`; before this, the provider made one request and reported the
// 429, so anything past about 20 resources on the free tier failed partway
// through an apply.
//
// Most of these drive do() directly with a recording `wait`, so a retry
// that should sit out a minute is checked in microseconds and the exact
// wait asked for can be asserted. One runs a real Terraform apply, to show
// the 429 reaches the user as the same diagnostic it always did once the
// retries run out.

// rateLimitMessage is the API's own 429 text, as the triage captured it.
const rateLimitMessage = "Rate limit exceeded, retry in 57 seconds"

// scriptedServer answers each request with the next status in `statuses`,
// repeating the last one once the script runs out. A 429 carries
// `retryAfter` as its Retry-After header (none when empty) and the API's
// error body; anything else is a JSON body with a message, so a 200 can be
// told apart from a 429 by more than its status. Every request body is
// recorded.
type scriptedServer struct {
	mu         sync.Mutex
	server     *httptest.Server
	statuses   []int
	retryAfter string
	bodies     []string
	// onRequest, when set, runs inside the handler before it answers.
	onRequest func()
}

func newScriptedServer(t *testing.T, retryAfter string, statuses ...int) *scriptedServer {
	t.Helper()
	s := &scriptedServer{statuses: statuses, retryAfter: retryAfter}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		n := len(s.bodies)
		status := s.statuses[len(s.statuses)-1]
		if n <= len(s.statuses) {
			status = s.statuses[n-1]
		}
		onRequest := s.onRequest
		s.mu.Unlock()
		if onRequest != nil {
			onRequest()
		}
		if status == http.StatusTooManyRequests {
			if s.retryAfter != "" {
				w.Header().Set("Retry-After", s.retryAfter)
			}
			writeAPIError(w, status, rateLimitMessage)
			return
		}
		writeJSON(w, status, map[string]any{"message": fmt.Sprintf("answered with %d", status)})
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *scriptedServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// recordingClient is a client for s whose waits are recorded, not slept,
// and whose clock is fixed at `now`.
func recordingClient(s *scriptedServer, now time.Time) (*nxipClient, *[]time.Duration) {
	waits := &[]time.Duration{}
	return &nxipClient{
		baseURL: s.server.URL,
		apiKey:  "key",
		http:    s.server.Client(),
		wait: func(ctx context.Context, d time.Duration) error {
			*waits = append(*waits, d)
			return nil
		},
		now: func() time.Time { return now },
	}, waits
}

// TestClient_retriesA429WithRetryAfterSeconds is the API's own shape:
// Retry-After in seconds. The request is sent again after that wait, with
// the same body (a POST body read once by the first send must not go out
// empty the second time), the 200 is what the caller gets, and the wait is
// logged where TF_LOG shows it.
func TestClient_retriesA429WithRetryAfterSeconds(t *testing.T) {
	s := newScriptedServer(t, "2", http.StatusTooManyRequests, http.StatusOK)
	client, waits := recordingClient(s, time.Now())

	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &logs)

	status, message, err := client.do(ctx, http.MethodPost, "/v1/subnets", map[string]any{"family": "IPV4"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK || message != "answered with 200" {
		t.Fatalf("want the retry's 200, got %d %q", status, message)
	}
	bodies := s.requests()
	if len(bodies) != 2 {
		t.Fatalf("want 2 requests (the 429 and its retry), got %d", len(bodies))
	}
	if bodies[0] != `{"family":"IPV4"}` || bodies[1] != bodies[0] {
		t.Fatalf("want the same body sent both times, got %q then %q", bodies[0], bodies[1])
	}
	if len(*waits) != 1 || (*waits)[0] != 2*time.Second {
		t.Fatalf("want one wait of 2s, got %v", *waits)
	}

	entries, err := tflogtest.MultilineJSONDecode(&logs)
	if err != nil {
		t.Fatalf("decoding logs: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry["@message"] == "nxip API rate limit reached, waiting before retrying" {
			found = true
			if entry["@level"] != "warn" || entry["wait_seconds"] != float64(2) || entry["path"] != "/v1/subnets" {
				t.Fatalf("rate-limit log entry has the wrong fields: %v", entry)
			}
		}
	}
	if !found {
		t.Fatalf("want a rate-limit warning in the provider log, got %v", entries)
	}
}

// TestClient_retriesA429WithRetryAfterDate: Retry-After may also be an HTTP
// date. The wait is the time from now until then.
func TestClient_retriesA429WithRetryAfterDate(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := newScriptedServer(t, now.Add(7*time.Second).Format(http.TimeFormat), http.StatusTooManyRequests, http.StatusOK)
	client, waits := recordingClient(s, now)

	status, _, err := client.do(context.Background(), http.MethodGet, "/v1/subnets/x", nil, nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("want the retry's 200, got %d (err %v)", status, err)
	}
	if len(s.requests()) != 2 {
		t.Fatalf("want 2 requests, got %d", len(s.requests()))
	}
	if len(*waits) != 1 || (*waits)[0] != 7*time.Second {
		t.Fatalf("want one wait of 7s, got %v", *waits)
	}
}

// TestClient_rateLimitAttemptCap: an API that keeps answering 429 gets the
// original request plus rateLimitMaxRetries retries, no more, and the
// caller gets the last 429 with the API's own message, which is what every
// resource turns into its diagnostic.
func TestClient_rateLimitAttemptCap(t *testing.T) {
	s := newScriptedServer(t, "1", http.StatusTooManyRequests)
	client, waits := recordingClient(s, time.Now())

	status, message, err := client.do(context.Background(), http.MethodGet, "/v1/pools", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusTooManyRequests || message != rateLimitMessage {
		t.Fatalf("want the 429 and its message, got %d %q", status, message)
	}
	if got := len(s.requests()); got != rateLimitMaxRetries+1 {
		t.Fatalf("want %d requests, got %d", rateLimitMaxRetries+1, got)
	}
	if len(*waits) != rateLimitMaxRetries {
		t.Fatalf("want %d waits, got %v", rateLimitMaxRetries, *waits)
	}
}

// TestClient_onlyA429IsRetried: a 500 on a POST may follow an allocation
// that committed, so sending it again could allocate a second block. It is
// returned at once.
func TestClient_onlyA429IsRetried(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := newScriptedServer(t, "1", status, http.StatusOK)
			client, waits := recordingClient(s, time.Now())

			got, _, err := client.do(context.Background(), http.MethodPost, "/v1/subnets", map[string]any{"family": "IPV4"}, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != status {
				t.Fatalf("want %d returned as is, got %d", status, got)
			}
			if n := len(s.requests()); n != 1 || len(*waits) != 0 {
				t.Fatalf("want 1 request and no wait, got %d requests and waits %v", n, *waits)
			}
		})
	}
}

// TestClient_rateLimitWaitStopsOnCancel: Ctrl+C during a wait ends it at
// once, with the real sleep rather than a recorded one, instead of sitting
// out a Retry-After of up to a minute.
func TestClient_rateLimitWaitStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newScriptedServer(t, "30", http.StatusTooManyRequests)
	// Cancelled a moment after the 429 is answered, not inside the
	// handler: cancelling there could fail the request itself, and the
	// test would pass without ever reaching the wait.
	s.onRequest = func() { time.AfterFunc(200*time.Millisecond, cancel) }
	client := &nxipClient{baseURL: s.server.URL, apiKey: "key", http: s.server.Client()}

	started := time.Now()
	_, _, err := client.do(ctx, http.MethodGet, "/v1/pools", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "stopped waiting for the nxip API rate limit to reset: context canceled") {
		t.Fatalf("want the wait to end with a context-canceled error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("want the wait abandoned at once, took %v", elapsed)
	}
}

// TestRetryAfterDelay covers the header forms beyond the two above: a wait
// longer than rateLimitMaxWait is capped, a date already passed means no
// wait, and anything unreadable or missing falls back to
// rateLimitDefaultWait.
func TestRetryAfterDelay(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"57", 57 * time.Second},
		{" 3 ", 3 * time.Second},
		{"0", 0},
		{"3600", rateLimitMaxWait},
		{now.Add(10 * time.Minute).Format(http.TimeFormat), rateLimitMaxWait},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"", rateLimitDefaultWait},
		{"-5", rateLimitDefaultWait},
		{"soon", rateLimitDefaultWait},
	}
	for _, c := range cases {
		if got := retryAfterDelay(c.header, now); got != c.want {
			t.Errorf("retryAfterDelay(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

// TestSubnetResource_rateLimitDiagnostic runs a real apply against an API
// that rate-limits every subnet create. Retry-After is 0 so the real wait
// is instant. Once the retries run out the user sees the same diagnostic
// as before retries existed, the API's sentence and the 429, and the
// create was sent exactly 1 + rateLimitMaxRetries times.
func TestSubnetResource_rateLimitDiagnostic(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/subnets" {
			mu.Lock()
			posts++
			mu.Unlock()
			w.Header().Set("Retry-After", "0")
			writeAPIError(w, http.StatusTooManyRequests, rateLimitMessage)
			return
		}
		writeAPIError(w, http.StatusNotFound, "Route not found")
	}))
	defer server.Close()

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      fakeSubnetConfig(server.URL, fakeSubnet{"s", nil}),
				ExpectError: regexp.MustCompile(`failed to create subnet: Rate limit exceeded, retry in 57\s+seconds\s+\(HTTP 429\)`),
			},
		},
	})

	mu.Lock()
	defer mu.Unlock()
	if posts != rateLimitMaxRetries+1 {
		t.Fatalf("want %d creates sent, got %d", rateLimitMaxRetries+1, posts)
	}
}
