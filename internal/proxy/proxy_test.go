package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/shreyashyadav1/switchyard/internal/auth"
	"github.com/shreyashyadav1/switchyard/internal/config"
	"github.com/shreyashyadav1/switchyard/internal/telemetry"
)

// upstream is a fake OpenAI-compatible provider that records what it got.
type upstream struct {
	*httptest.Server
	calls   atomic.Int32
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
	respond func(w http.ResponseWriter, call int, body map[string]any)
}

func newUpstream(t testing.TB, respond func(w http.ResponseWriter, call int, body map[string]any)) *upstream {
	u := &upstream{respond: respond}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		n := int(u.calls.Add(1))
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		u.headers = append(u.headers, r.Header.Clone())
		u.mu.Unlock()
		u.respond(w, n, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) last() (map[string]any, http.Header) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[len(u.bodies)-1], u.headers[len(u.headers)-1]
}

func completion(w http.ResponseWriter, in, out int) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, in, out)
}

func status(code int) func(http.ResponseWriter, int, map[string]any) {
	return func(w http.ResponseWriter, _ int, _ map[string]any) {
		http.Error(w, `{"error":{"message":"nope"}}`, code)
	}
}

type harness struct {
	gw     *Gateway
	srv    *httptest.Server
	token  string
	sleeps []time.Duration
}

// newHarness starts a gateway whose "chat" model falls back from primary to
// secondary. extra is appended to the YAML.
func newHarness(t testing.TB, primary, secondary *upstream, extra string) *harness {
	t.Helper()
	token, digest, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf(`
providers:
  primary: {base_url: %q, api_key_env: PRIMARY_KEY}
  secondary: {base_url: %q}
models:
  - name: chat
    targets:
      - {provider: primary, model: big-model, price: {input: 1.0, output: 2.0}}
      - {provider: secondary, model: small-model, price: {input: 0.5, output: 0.5}}
  - name: embed
    targets:
      - {provider: primary, model: embed-model, price: {input: 0.1}}
retry: {max_attempts: 2, base_delay: 10ms, max_delay: 1s}
keys:
  - {name: test, sha256: %s}
%s`, primary.URL+"/v1", secondary.URL+"/v1/", digest, extra)
	cfg, err := config.Parse([]byte(yaml), func(k string) (string, bool) {
		return "sk-primary", k == "PRIMARY_KEY"
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{token: token}
	h.gw = New(cfg, telemetry.NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.gw.sleep = func(_ context.Context, d time.Duration) error {
		h.sleeps = append(h.sleeps, d)
		return nil
	}
	h.srv = httptest.NewServer(h.gw.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) post(t *testing.T, path, body string, header ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.token)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	return e.Error.Code
}

const chatBody = `{"model":"chat","messages":[{"role":"user","content":"hi"}],"seed":12345678901234567}`

func TestForwardsToFirstTarget(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 1000, 500) })
	secondary := newUpstream(t, status(500))
	h := newHarness(t, primary, secondary, "")

	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
	}
	if got := resp.Header.Get(headerTarget); got != "primary/big-model" {
		t.Errorf("target header = %q", got)
	}
	// 1000 input tokens at $1/M + 500 output tokens at $2/M.
	if got := resp.Header.Get(headerCost); got != "0.002" {
		t.Errorf("cost header = %q, want 0.002", got)
	}
	body, hdr := primary.last()
	if body["model"] != "big-model" {
		t.Errorf("model not rewritten: %v", body["model"])
	}
	if got := hdr.Get("Authorization"); got != "Bearer sk-primary" {
		t.Errorf("upstream auth = %q: the provider key must replace the client key", got)
	}
	if _, ok := body["stream_options"]; ok {
		t.Error("non-streaming requests must not get stream_options")
	}
	if got := h.gw.ledger.Spent("test"); got != 0.002 {
		t.Errorf("ledger = %v", got)
	}
	if secondary.calls.Load() != 0 {
		t.Error("secondary should not be called")
	}
}

func TestPreservesLargeIntegers(t *testing.T) {
	var raw string
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 1, 1) })
	primary.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		completion(w, 1, 1)
	})
	h := newHarness(t, primary, newUpstream(t, status(500)), "")
	h.post(t, "/v1/chat/completions", chatBody)
	if !strings.Contains(raw, `"seed":12345678901234567`) {
		t.Errorf("seed changed on the way through: %s", raw)
	}
}

func TestRetriesTransientErrorThenSucceeds(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, call int, _ map[string]any) {
		if call == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		completion(w, 10, 10)
	})
	secondary := newUpstream(t, status(500))
	h := newHarness(t, primary, secondary, "")

	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != 200 || resp.Header.Get(headerTarget) != "primary/big-model" {
		t.Fatalf("status %d target %q", resp.StatusCode, resp.Header.Get(headerTarget))
	}
	if primary.calls.Load() != 2 || secondary.calls.Load() != 0 {
		t.Errorf("calls: primary %d secondary %d", primary.calls.Load(), secondary.calls.Load())
	}
	if len(h.sleeps) != 1 || h.sleeps[0] < 5*time.Millisecond || h.sleeps[0] > 10*time.Millisecond {
		t.Errorf("backoff sleeps = %v, want one in [5ms, 10ms]", h.sleeps)
	}
}

func TestFallsBackAfterRetriesRunOut(t *testing.T) {
	primary := newUpstream(t, status(http.StatusBadGateway))
	secondary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 100, 100) })
	h := newHarness(t, primary, secondary, "")

	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != 200 || resp.Header.Get(headerTarget) != "secondary/small-model" {
		t.Fatalf("status %d target %q", resp.StatusCode, resp.Header.Get(headerTarget))
	}
	if primary.calls.Load() != 2 {
		t.Errorf("primary should be tried max_attempts=2 times, got %d", primary.calls.Load())
	}
	body, hdr := secondary.last()
	if body["model"] != "small-model" {
		t.Errorf("fallback model = %v", body["model"])
	}
	if hdr.Get("Authorization") != "" {
		t.Error("a provider without api_key_env must get no Authorization header")
	}
	attempts := h.gw.metrics.Attempts
	if got := testutil.ToFloat64(attempts.WithLabelValues("primary/big-model", "retryable")); got != 2 {
		t.Errorf("retryable attempts metric = %v", got)
	}
}

func TestFallsBackImmediatelyOnAuthError(t *testing.T) {
	primary := newUpstream(t, status(http.StatusUnauthorized))
	secondary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 1, 1) })
	h := newHarness(t, primary, secondary, "")

	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != 200 || primary.calls.Load() != 1 || len(h.sleeps) != 0 {
		t.Fatalf("status %d, primary calls %d, sleeps %v", resp.StatusCode, primary.calls.Load(), h.sleeps)
	}
}

func TestLongRetryAfterSkipsToNextTarget(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})
	secondary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 1, 1) })
	h := newHarness(t, primary, secondary, "")

	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != 200 || primary.calls.Load() != 1 || len(h.sleeps) != 0 {
		t.Fatalf("status %d, primary calls %d, sleeps %v", resp.StatusCode, primary.calls.Load(), h.sleeps)
	}
}

func TestShortRetryAfterIsHonored(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, call int, _ map[string]any) {
		if call == 1 {
			w.Header().Set("Retry-After", "0.5")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		completion(w, 1, 1)
	})
	h := newHarness(t, primary, newUpstream(t, status(500)), "")

	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != 200 || len(h.sleeps) != 1 || h.sleeps[0] != 500*time.Millisecond {
		t.Fatalf("status %d, sleeps %v", resp.StatusCode, h.sleeps)
	}
}

func TestClientErrorIsRelayedWithoutFallback(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad messages","code":"invalid_messages"}}`)
	})
	secondary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 1, 1) })
	h := newHarness(t, primary, secondary, "")

	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != 400 || errorCode(t, resp) != "invalid_messages" {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if secondary.calls.Load() != 0 || primary.calls.Load() != 1 {
		t.Error("a bad request must not be retried or sent to another target")
	}
}

func TestAllTargetsFailing(t *testing.T) {
	h := newHarness(t, newUpstream(t, status(500)), newUpstream(t, status(404)), "")
	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, resp) != "all_targets_failed" {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestStreamingPassesChunksAndCountsUsage(t *testing.T) {
	chunks := []string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":2000,"completion_tokens":1000}}`,
		`data: [DONE]`,
	}
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
			w.(http.Flusher).Flush()
		}
	})
	h := newHarness(t, primary, newUpstream(t, status(500)), "")

	resp := h.post(t, "/v1/chat/completions",
		`{"model":"chat","stream":true,"stream_options":{"foo":1},"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var got []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			got = append(got, line)
		}
	}
	if strings.Join(got, "\n") != strings.Join(chunks, "\n") {
		t.Errorf("client got:\n%s", strings.Join(got, "\n"))
	}
	body, _ := primary.last()
	opts, _ := body["stream_options"].(map[string]any)
	if opts["include_usage"] != true || opts["foo"] != float64(1) {
		t.Errorf("stream_options = %v: include_usage must be added and client options kept", opts)
	}
	// 2000 × $1/M + 1000 × $2/M
	if spent := h.gw.ledger.Spent("test"); spent != 0.004 {
		t.Errorf("ledger = %v, want 0.004", spent)
	}
}

func TestCache(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 10, 10) })
	h := newHarness(t, primary, newUpstream(t, status(500)), "cache: {enabled: true}")
	deterministic := `{"model":"chat","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

	first := h.post(t, "/v1/chat/completions", deterministic)
	firstBody := readAll(t, first)
	if first.Header.Get(headerCache) != "miss" {
		t.Errorf("first request cache header = %q", first.Header.Get(headerCache))
	}
	second := h.post(t, "/v1/chat/completions", deterministic)
	if second.Header.Get(headerCache) != "hit" || readAll(t, second) != firstBody {
		t.Error("identical temperature-0 request should be served from cache")
	}
	if second.Header.Get(headerCost) != "0" || second.Header.Get(headerTarget) != "primary/big-model" {
		t.Errorf("hit headers: cost %q target %q", second.Header.Get(headerCost), second.Header.Get(headerTarget))
	}
	if primary.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", primary.calls.Load())
	}

	sampled := h.post(t, "/v1/chat/completions", `{"model":"chat","temperature":0.7,"messages":[]}`)
	if sampled.Header.Get(headerCache) != "" {
		t.Error("sampled requests must bypass the cache")
	}
	h.post(t, "/v1/embeddings", `{"model":"embed","input":"x"}`)
	if h.post(t, "/v1/embeddings", `{"model":"embed","input":"x"}`).Header.Get(headerCache) != "hit" {
		t.Error("embeddings are deterministic and should be cached")
	}
	if got := testutil.ToFloat64(h.gw.metrics.Requests.WithLabelValues("chat", "200", "hit")); got != 1 {
		t.Errorf("cache hit metric = %v", got)
	}
}

func TestCacheableRules(t *testing.T) {
	cases := []struct {
		body   string
		stream bool
		want   bool
	}{
		{`{"temperature":0}`, false, true},
		{`{"temperature":0.0}`, false, true},
		{`{"temperature":0}`, true, false},
		{`{"temperature":0.2}`, false, false},
		{`{}`, false, false},
		{`{"temperature":0,"n":3}`, false, false},
	}
	for _, tc := range cases {
		dec := json.NewDecoder(strings.NewReader(tc.body))
		dec.UseNumber()
		var req map[string]any
		_ = dec.Decode(&req)
		if got := cacheable(chatPath, req, tc.stream); got != tc.want {
			t.Errorf("%s stream=%v: got %v", tc.body, tc.stream, got)
		}
	}
}

func TestRateLimit(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 1, 1) })
	token, digest, _ := auth.NewToken()
	h := newHarness(t, primary, newUpstream(t, status(500)),
		fmt.Sprintf("  - {name: slow, sha256: %s, requests_per_minute: 1}", digest))
	h.token = token

	if resp := h.post(t, "/v1/chat/completions", chatBody); resp.StatusCode != 200 {
		t.Fatalf("first request status %d", resp.StatusCode)
	}
	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "60" || errorCode(t, resp) != "rate_limit_exceeded" {
		t.Fatalf("status %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestDailyBudget(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 1_000_000, 0) })
	token, digest, _ := auth.NewToken()
	h := newHarness(t, primary, newUpstream(t, status(500)),
		fmt.Sprintf("  - {name: capped, sha256: %s, daily_budget_usd: 0.5}", digest))
	h.token = token

	if resp := h.post(t, "/v1/chat/completions", chatBody); resp.StatusCode != 200 {
		t.Fatalf("first request status %d", resp.StatusCode) // costs $1, over the $0.50 budget
	}
	resp := h.post(t, "/v1/chat/completions", chatBody)
	if resp.StatusCode != 429 || errorCode(t, resp) != "insufficient_quota" {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if primary.calls.Load() != 1 {
		t.Error("a request over budget must not reach the upstream")
	}
}

func TestRequestValidation(t *testing.T) {
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 1, 1) })
	h := newHarness(t, primary, newUpstream(t, status(500)), "max_body_bytes: 200")

	cases := []struct {
		name, body, token string
		status            int
		code              string
	}{
		{"missing key", chatBody, "", 401, "invalid_api_key"},
		{"wrong key", chatBody, "sy_wrong", 401, "invalid_api_key"},
		{"unknown model", `{"model":"gpt-9"}`, h.token, 404, "model_not_found"},
		{"not json", `hello`, h.token, 400, "invalid_json"},
		{"too large", `{"model":"chat","x":"` + strings.Repeat("a", 300) + `"}`, h.token, 413, "request_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.token = tc.token
			resp := h.post(t, "/v1/chat/completions", tc.body)
			if resp.StatusCode != tc.status || errorCode(t, resp) != tc.code {
				t.Fatalf("status %d", resp.StatusCode)
			}
		})
	}
	if primary.calls.Load() != 0 {
		t.Error("invalid requests must not reach the upstream")
	}
}

func TestTraceContextPropagates(t *testing.T) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	primary := newUpstream(t, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 1, 1) })
	h := newHarness(t, primary, newUpstream(t, status(500)), "")

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	h.post(t, "/v1/chat/completions", chatBody, "traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	_, hdr := primary.last()
	if got := hdr.Get("traceparent"); !strings.Contains(got, traceID) {
		t.Errorf("upstream traceparent = %q, want trace id %s", got, traceID)
	}
}

func TestListModels(t *testing.T) {
	h := newHarness(t, newUpstream(t, status(500)), newUpstream(t, status(500)), "")
	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 2 || out.Data[0].ID != "chat" || out.Data[1].ID != "embed" {
		t.Errorf("models = %+v", out.Data)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"2", 2 * time.Second, true},
		{"0.25", 250 * time.Millisecond, true},
		{now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second, true},
		{"", 0, false},
		{"soon", 0, false},
	}
	for _, tc := range cases {
		got, ok := retryAfter(tc.in, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: got %v %v", tc.in, got, ok)
		}
	}
}
