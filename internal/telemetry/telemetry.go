// Package telemetry sets up Prometheus metrics and OpenTelemetry tracing.
package telemetry

import (
	"context"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Metrics are the gateway's Prometheus series.
type Metrics struct {
	Registry *prometheus.Registry

	// Requests counts client requests by model name, status code and cache
	// result (hit, miss or bypass).
	Requests *prometheus.CounterVec
	// Attempts counts upstream calls by target and outcome.
	Attempts *prometheus.CounterVec
	Tokens   *prometheus.CounterVec
	CostUSD  *prometheus.CounterVec
	Duration *prometheus.HistogramVec
	// BreakerState is each target's circuit breaker: 0 closed, 1 open, 2 half-open.
	BreakerState *prometheus.GaugeVec
	// BreakerSkips counts requests that skipped a target because its breaker was open.
	BreakerSkips *prometheus.CounterVec
}

// NewMetrics registers the gateway's series on a fresh registry.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := &Metrics{
		Registry: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchyard_requests_total",
			Help: "Client requests by model name, HTTP status and cache result.",
		}, []string{"model", "status", "cache"}),
		Attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchyard_upstream_attempts_total",
			Help: "Upstream calls by target and outcome.",
		}, []string{"target", "outcome"}),
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchyard_tokens_total",
			Help: "Tokens reported by upstreams, by target and direction.",
		}, []string{"target", "direction"}),
		CostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchyard_cost_usd_total",
			Help: "Estimated spend in US dollars by client key and target.",
		}, []string{"key", "target"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "switchyard_request_duration_seconds",
			Help:    "Client request duration, including retries, fallbacks and streaming.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"model", "stream"}),
		BreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "switchyard_breaker_state",
			Help: "Circuit breaker state by target: 0 closed, 1 open, 2 half-open.",
		}, []string{"target"}),
		BreakerSkips: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "switchyard_breaker_skips_total",
			Help: "Requests that skipped a target because its circuit breaker was open.",
		}, []string{"target"}),
	}
	reg.MustRegister(m.Requests, m.Attempts, m.Tokens, m.CostUSD, m.Duration, m.BreakerState, m.BreakerSkips)
	return m
}

// SetupTracing installs the W3C trace-context propagator and, when
// OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set,
// an OTLP/HTTP exporter. The returned function flushes and stops it.
func SetupTracing(ctx context.Context, version string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", serviceName()),
		attribute.String("service.version", version),
	))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func serviceName() string {
	if n := os.Getenv("OTEL_SERVICE_NAME"); n != "" {
		return n
	}
	return "switchyard"
}
