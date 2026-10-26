// Package proxy is the gateway's HTTP handler: it authenticates and limits
// clients, resolves model names to targets, and forwards requests with
// retries, fallbacks, caching and cost accounting.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/shreyashyadav1/switchyard/internal/auth"
	"github.com/shreyashyadav1/switchyard/internal/breaker"
	"github.com/shreyashyadav1/switchyard/internal/cache"
	"github.com/shreyashyadav1/switchyard/internal/config"
	"github.com/shreyashyadav1/switchyard/internal/ledger"
	"github.com/shreyashyadav1/switchyard/internal/ratelimit"
	"github.com/shreyashyadav1/switchyard/internal/telemetry"
	"github.com/shreyashyadav1/switchyard/internal/usage"
)

const (
	chatPath       = "/chat/completions"
	embeddingsPath = "/embeddings"

	maxResponseBytes = 64 << 20
	maxErrorBytes    = 1 << 20

	headerTarget = "X-Switchyard-Target"
	headerCache  = "X-Switchyard-Cache"
	headerCost   = "X-Switchyard-Cost-Usd"
)

// Gateway serves the OpenAI-compatible API.
type Gateway struct {
	cfg     *config.Config
	models  map[string]config.Model
	client  *http.Client
	auth    *auth.Authenticator
	limiter *ratelimit.Limiter
	ledger  *ledger.Ledger
	cache   *cache.Cache
	// breakers is nil when circuit breaking is disabled.
	breakers *breaker.Set
	metrics  *telemetry.Metrics
	tracer   trace.Tracer
	log      *slog.Logger

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// New builds a Gateway from a validated config.
func New(cfg *config.Config, metrics *telemetry.Metrics, log *slog.Logger) *Gateway {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = cfg.UpstreamTimeout
	transport.MaxIdleConnsPerHost = 32

	g := &Gateway{
		cfg:     cfg,
		models:  make(map[string]config.Model, len(cfg.Models)),
		client:  &http.Client{Transport: transport},
		auth:    auth.New(cfg.Keys, cfg.AllowAnonymous),
		limiter: ratelimit.New(nil),
		ledger:  ledger.New(nil),
		metrics: metrics,
		tracer:  otel.Tracer("github.com/shreyashyadav1/switchyard"),
		log:     log,
		now:     time.Now,
		sleep:   sleepCtx,
	}
	for _, m := range cfg.Models {
		g.models[m.Name] = m
	}
	if cfg.Cache.Enabled {
		g.cache = cache.New(cfg.Cache.TTL, cfg.Cache.MaxEntries, nil)
	}
	if !cfg.Breaker.Disabled {
		// g.now is read on each call so tests can swap the clock after New.
		g.breakers = breaker.New(cfg.Breaker.FailureThreshold, cfg.Breaker.Cooldown,
			func() time.Time { return g.now() }, g.breakerChanged)
		for _, m := range cfg.Models {
			for _, t := range m.Targets {
				metrics.BreakerState.WithLabelValues(t.Label()).Set(float64(breaker.Closed))
			}
		}
	}
	return g
}

// Handler returns the client-facing routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	// The same API under /openai/v1, the path Groq's SDKs use, so clients built on
	// them only need their base URL pointed at the gateway.
	for _, prefix := range []string{"/v1", "/openai/v1"} {
		mux.HandleFunc("GET "+prefix+"/models", g.listModels)
		mux.HandleFunc("POST "+prefix+chatPath, g.forward(chatPath))
		mux.HandleFunc("POST "+prefix+embeddingsPath, g.forward(embeddingsPath))
	}
	return mux
}

// MetricsHandler serves Prometheus metrics. It is meant for a separate,
// private listener because the series include client key names and spend.
func (g *Gateway) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(g.metrics.Registry, promhttp.HandlerOpts{})
}

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.auth.Authenticate(r); !ok {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "Missing or invalid API key.")
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}
	for _, m := range g.cfg.Models {
		out.Data = append(out.Data, model{ID: m.Name, Object: "model", OwnedBy: "switchyard"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// exchange collects what happened during one client request, for the access
// log, metrics and the server span.
type exchange struct {
	key      string
	model    string
	target   string
	cache    string
	stream   bool
	attempts int
	usage    usage.Usage
	cost     float64
	err      error
}

func (g *Gateway) forward(endpoint string) http.HandlerFunc {
	operation := "chat"
	if endpoint == embeddingsPath {
		operation = "embeddings"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		start := g.now()
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := g.tracer.Start(ctx, "POST /v1"+endpoint, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		ctx, cancel := context.WithTimeout(ctx, g.cfg.RequestTimeout)
		defer cancel()

		ex := &exchange{model: "unknown", cache: "bypass"}
		sw := &statusWriter{ResponseWriter: w}
		defer func() { g.finish(ctx, span, ex, sw.status, start) }()

		id, ok := g.auth.Authenticate(r)
		if !ok {
			writeError(sw, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "Missing or invalid API key.")
			return
		}
		ex.key = id.Name
		if ok, wait := g.limiter.Allow(id.Name, id.RequestsPerMinute); !ok {
			sw.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeError(sw, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
				fmt.Sprintf("Rate limit of %d requests per minute exceeded.", id.RequestsPerMinute))
			return
		}
		if id.DailyBudgetUSD > 0 && g.ledger.Spent(id.Name) >= id.DailyBudgetUSD {
			writeError(sw, http.StatusTooManyRequests, "insufficient_quota", "insufficient_quota",
				fmt.Sprintf("Daily budget of $%.2f is used up; it resets at 00:00 UTC.", id.DailyBudgetUSD))
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(sw, r.Body, g.cfg.MaxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(sw, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large",
					fmt.Sprintf("Request body exceeds %d bytes.", g.cfg.MaxBodyBytes))
				return
			}
			writeError(sw, http.StatusBadRequest, "invalid_request_error", "invalid_body", "Could not read request body.")
			return
		}
		var req map[string]any
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber() // keep numbers such as seeds exact when re-encoding
		if err := dec.Decode(&req); err != nil {
			writeError(sw, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Request body must be a JSON object.")
			return
		}
		name, _ := req["model"].(string)
		model, ok := g.models[name]
		if !ok {
			writeError(sw, http.StatusNotFound, "invalid_request_error", "model_not_found",
				fmt.Sprintf("Model %q is not configured on this gateway.", name))
			return
		}
		ex.model = name
		ex.stream = endpoint == chatPath && req["stream"] == true
		span.SetAttributes(
			attribute.String("gen_ai.operation.name", operation),
			attribute.String("gen_ai.request.model", name),
			attribute.String("switchyard.key", id.Name),
			attribute.Bool("switchyard.stream", ex.stream),
		)

		var cacheKey string
		if g.cache != nil && cacheable(endpoint, req, ex.stream) {
			ex.cache = "miss"
			cacheKey = keyFor(id.Name, endpoint, req)
			if hit, ok := g.cache.Get(cacheKey); ok {
				ex.cache, ex.target = "hit", hit.Target
				sw.Header().Set("Content-Type", hit.ContentType)
				sw.Header().Set(headerTarget, hit.Target)
				sw.Header().Set(headerCache, "hit")
				sw.Header().Set(headerCost, "0")
				sw.WriteHeader(http.StatusOK)
				_, _ = sw.Write(hit.Body)
				return
			}
		}

		var lastErr error
		skipped := 0
		var retryIn time.Duration
		for _, t := range model.Targets {
			if ok, wait := g.allowTarget(span, t); !ok {
				skipped++
				if retryIn == 0 || wait < retryIn {
					retryIn = wait
				}
				lastErr = fmt.Errorf("%s: circuit open", t.Label())
				continue
			}
			resp, err := g.call(ctx, operation, endpoint, t, req, ex)
			g.reportTarget(ctx, t, err)
			if err != nil {
				lastErr = err
				if ctx.Err() != nil {
					break
				}
				continue
			}
			defer resp.Body.Close()
			ex.target = t.Label()
			switch {
			case resp.StatusCode/100 != 2:
				g.relayError(sw, resp, t)
			case ex.stream:
				g.relayStream(sw, resp, t, id, ex)
			default:
				g.relayBuffered(sw, resp, t, id, ex, cacheKey)
			}
			return
		}

		ex.err = lastErr
		if skipped == len(model.Targets) {
			// Every target is known to be down: answer now rather than retrying them.
			sw.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retryIn.Seconds())))))
			writeError(sw, http.StatusServiceUnavailable, "upstream_error", "all_targets_unavailable",
				fmt.Sprintf("Every target for %q is failing and temporarily skipped; try again later.", name))
			return
		}
		if ctx.Err() != nil {
			writeError(sw, http.StatusGatewayTimeout, "upstream_error", "upstream_timeout",
				"The request timed out before any target answered.")
			return
		}
		writeError(sw, http.StatusBadGateway, "upstream_error", "all_targets_failed",
			fmt.Sprintf("All %d targets for %q failed; last error: %v", len(model.Targets), name, lastErr))
	}
}

type outcome int

const (
	outcomeOK          outcome = iota
	outcomeClientError         // the request itself is bad: relay, don't fall back
	outcomeRetry               // transient: retry, then fall back
	outcomeFallback            // this target can't serve it: fall back now
)

func classify(status int) outcome {
	switch {
	case status/100 == 2:
		return outcomeOK
	case status == http.StatusBadRequest, status == http.StatusRequestEntityTooLarge, status == http.StatusUnprocessableEntity:
		return outcomeClientError
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return outcomeRetry
	default: // 401, 403, 404 and the rest point at this target's config
		return outcomeFallback
	}
}

// call sends the request to one target, retrying transient failures. It
// returns a response for success or client errors, and an error when the
// caller should fall back to the next target.
func (g *Gateway) call(ctx context.Context, operation, endpoint string, t config.Target, req map[string]any, ex *exchange) (*http.Response, error) {
	payload, err := payloadFor(req, t.Model, ex.stream)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 1; ; attempt++ {
		ex.attempts++
		resp, err := g.attempt(ctx, operation, endpoint, t, payload, attempt)
		var delay time.Duration
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			g.metrics.Attempts.WithLabelValues(t.Label(), "network_error").Inc()
			lastErr = fmt.Errorf("%s: %w", t.Label(), err)
			delay = g.backoff(attempt)
		} else {
			switch classify(resp.StatusCode) {
			case outcomeOK:
				g.metrics.Attempts.WithLabelValues(t.Label(), "ok").Inc()
				return resp, nil
			case outcomeClientError:
				g.metrics.Attempts.WithLabelValues(t.Label(), "client_error").Inc()
				return resp, nil
			case outcomeFallback:
				g.metrics.Attempts.WithLabelValues(t.Label(), "fallback").Inc()
				drain(resp)
				return nil, fmt.Errorf("%s returned HTTP %d", t.Label(), resp.StatusCode)
			case outcomeRetry:
				g.metrics.Attempts.WithLabelValues(t.Label(), "retryable").Inc()
				lastErr = fmt.Errorf("%s returned HTTP %d", t.Label(), resp.StatusCode)
				delay = g.backoff(attempt)
				if ra, ok := retryAfter(resp.Header.Get("Retry-After"), g.now()); ok {
					if ra > g.cfg.Retry.MaxDelay {
						// Waiting that long on one target is worse than falling back.
						drain(resp)
						return nil, lastErr
					}
					delay = ra
				}
				drain(resp)
			}
		}
		if attempt >= g.cfg.Retry.MaxAttempts {
			return nil, lastErr
		}
		if err := g.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// allowTarget asks t's circuit breaker whether to try it, and records a skip.
func (g *Gateway) allowTarget(span trace.Span, t config.Target) (bool, time.Duration) {
	if g.breakers == nil {
		return true, 0
	}
	ok, wait := g.breakers.Allow(t.Label())
	if !ok {
		g.metrics.BreakerSkips.WithLabelValues(t.Label()).Inc()
		span.AddEvent("switchyard.breaker.skip", trace.WithAttributes(attribute.String("switchyard.target", t.Label())))
	}
	return ok, wait
}

// reportTarget feeds the result of call into t's circuit breaker. A response
// of any kind, client errors included, means the target is up. Failures
// caused by the client leaving or the request deadline say nothing about it.
func (g *Gateway) reportTarget(ctx context.Context, t config.Target, err error) {
	switch {
	case g.breakers == nil:
	case err == nil:
		g.breakers.Success(t.Label())
	case ctx.Err() != nil:
		g.breakers.Release(t.Label())
	default:
		g.breakers.Failure(t.Label())
	}
}

func (g *Gateway) breakerChanged(target string, from, to breaker.State) {
	g.metrics.BreakerState.WithLabelValues(target).Set(float64(to))
	g.log.Warn("circuit breaker", slog.String("target", target),
		slog.String("from", from.String()), slog.String("to", to.String()))
}

func (g *Gateway) attempt(ctx context.Context, operation, endpoint string, t config.Target, payload []byte, n int) (*http.Response, error) {
	p := g.cfg.Providers[t.Provider]
	target := strings.TrimRight(p.BaseURL, "/") + endpoint
	ctx, span := g.tracer.Start(ctx, operation+" "+t.Model, trace.WithSpanKind(trace.SpanKindClient))
	defer span.End()
	span.SetAttributes(
		attribute.String("gen_ai.operation.name", operation),
		attribute.String("gen_ai.provider.name", t.Provider),
		attribute.String("gen_ai.request.model", t.Model),
		attribute.Int("switchyard.attempt", n),
	)
	if u, err := url.Parse(target); err == nil {
		span.SetAttributes(attribute.String("server.address", u.Hostname()))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := g.client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "request failed")
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
	}
	return resp, nil
}

func (g *Gateway) relayError(w http.ResponseWriter, resp *http.Response, t config.Target) {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set(headerTarget, t.Label())
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

func (g *Gateway) relayBuffered(w http.ResponseWriter, resp *http.Response, t config.Target, id auth.Identity, ex *exchange, cacheKey string) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		ex.err = err
		writeError(w, http.StatusBadGateway, "upstream_error", "upstream_read_failed", "Could not read the upstream response.")
		return
	}
	u, _ := usage.Parse(data)
	g.record(id, t, u, ex)

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set(headerTarget, t.Label())
	w.Header().Set(headerCost, strconv.FormatFloat(ex.cost, 'f', -1, 64))
	if cacheKey != "" {
		w.Header().Set(headerCache, "miss")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)

	if cacheKey != "" && resp.StatusCode == http.StatusOK {
		g.cache.Put(cacheKey, cache.Entry{Body: data, ContentType: contentType, Target: t.Label()})
	}
}

func (g *Gateway) relayStream(w http.ResponseWriter, resp *http.Response, t config.Target, id auth.Identity, ex *exchange) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/event-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set(headerTarget, t.Label())
	w.WriteHeader(resp.StatusCode)

	rc := http.NewResponseController(w)
	br := bufio.NewReaderSize(resp.Body, 32<<10)
	var u usage.Usage
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if parsed, ok := usage.ParseSSELine(line); ok {
				u = parsed
			}
			if _, werr := w.Write(line); werr != nil {
				ex.err = werr
				break
			}
			_ = rc.Flush()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				ex.err = err
			}
			break
		}
	}
	// Record even a cut-off stream: the upstream has already billed the tokens.
	g.record(id, t, u, ex)
}

func (g *Gateway) record(id auth.Identity, t config.Target, u usage.Usage, ex *exchange) {
	ex.usage = u
	ex.cost = usage.Cost(t.Price, u)
	g.ledger.Add(id.Name, ex.cost)
	g.metrics.Tokens.WithLabelValues(t.Label(), "input").Add(float64(u.InputTokens))
	g.metrics.Tokens.WithLabelValues(t.Label(), "output").Add(float64(u.OutputTokens))
	g.metrics.CostUSD.WithLabelValues(id.Name, t.Label()).Add(ex.cost)
}

func (g *Gateway) finish(ctx context.Context, span trace.Span, ex *exchange, status int, start time.Time) {
	if status == 0 {
		status = 499 // nothing was written: the client went away
	}
	elapsed := g.now().Sub(start)
	g.metrics.Requests.WithLabelValues(ex.model, strconv.Itoa(status), ex.cache).Inc()
	g.metrics.Duration.WithLabelValues(ex.model, strconv.FormatBool(ex.stream)).Observe(elapsed.Seconds())

	span.SetAttributes(
		attribute.Int("http.response.status_code", status),
		attribute.String("switchyard.target", ex.target),
		attribute.String("switchyard.cache", ex.cache),
		attribute.Int("switchyard.attempts", ex.attempts),
		attribute.Int64("gen_ai.usage.input_tokens", ex.usage.InputTokens),
		attribute.Int64("gen_ai.usage.output_tokens", ex.usage.OutputTokens),
		attribute.Float64("switchyard.cost_usd", ex.cost),
	)
	if status >= 500 {
		span.SetStatus(codes.Error, http.StatusText(status))
	}

	attrs := []slog.Attr{
		slog.String("key", ex.key),
		slog.String("model", ex.model),
		slog.String("target", ex.target),
		slog.Int("status", status),
		slog.Bool("stream", ex.stream),
		slog.String("cache", ex.cache),
		slog.Int("attempts", ex.attempts),
		slog.Int64("input_tokens", ex.usage.InputTokens),
		slog.Int64("output_tokens", ex.usage.OutputTokens),
		slog.Float64("cost_usd", ex.cost),
		slog.Int64("duration_ms", elapsed.Milliseconds()),
	}
	if sc := span.SpanContext(); sc.HasTraceID() {
		attrs = append(attrs, slog.String("trace_id", sc.TraceID().String()))
	}
	level := slog.LevelInfo
	if ex.err != nil {
		attrs = append(attrs, slog.String("error", ex.err.Error()))
	}
	if status >= 500 {
		level = slog.LevelWarn
	}
	g.log.LogAttrs(ctx, level, "request", attrs...)
}

// backoff is exponential with equal jitter: half the delay is fixed, half is
// random, so retries from many clients spread out.
func (g *Gateway) backoff(attempt int) time.Duration {
	d := g.cfg.Retry.BaseDelay << (attempt - 1)
	if d <= 0 || d > g.cfg.Retry.MaxDelay {
		d = g.cfg.Retry.MaxDelay
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// retryAfter parses a Retry-After header given in seconds or as an HTTP date.
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
		return time.Duration(s * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// payloadFor rewrites the client's request for one target: the model name
// becomes the target's, and streaming requests ask for a usage chunk so the
// gateway can count tokens.
func payloadFor(req map[string]any, model string, stream bool) ([]byte, error) {
	out := make(map[string]any, len(req)+1)
	for k, v := range req {
		out[k] = v
	}
	out["model"] = model
	if stream {
		opts := map[string]any{}
		if existing, ok := req["stream_options"].(map[string]any); ok {
			for k, v := range existing {
				opts[k] = v
			}
		}
		opts["include_usage"] = true
		out["stream_options"] = opts
	}
	return json.Marshal(out)
}

// cacheable reports whether a response is safe to reuse for an identical
// request: embeddings always, chat only when sampling is turned off.
func cacheable(endpoint string, req map[string]any, stream bool) bool {
	if endpoint == embeddingsPath {
		return true
	}
	if stream {
		return false
	}
	temp, ok := req["temperature"].(json.Number)
	if !ok {
		return false
	}
	f, err := temp.Float64()
	if err != nil || f != 0 {
		return false
	}
	if n, ok := req["n"].(json.Number); ok && n.String() != "1" {
		return false
	}
	return true
}

// keyFor scopes cache entries to the client key, so one client's cached
// answers are never served to another.
func keyFor(client, endpoint string, req map[string]any) string {
	canonical, _ := json.Marshal(req) // map keys are sorted
	h := sha256.New()
	h.Write([]byte(client))
	h.Write([]byte{0})
	h.Write([]byte(endpoint))
	h.Write([]byte{0})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func writeError(w http.ResponseWriter, status int, typ, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": typ, "code": code},
	})
}

// statusWriter remembers the status code written to the client.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer's Flush.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
