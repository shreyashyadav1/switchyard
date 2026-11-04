# Benchmarks

## Gateway overhead (2026-11-04)

How much latency Switchyard adds to a chat completion, measured against a local fake upstream
so provider latency and network jitter are left out.

- Command: `go test -run '^$' -bench . -benchmem -count=10 ./internal/proxy`
- Code: `internal/proxy/bench_test.go` at commit 2c325bc (gateway code as on main at 2232b74)
- Machine: Apple M3, 8 cores, macOS, go1.27.3; load average 1.5 before the run
- Raw output: [benchmarks/2026-11-04-overhead.txt](benchmarks/2026-11-04-overhead.txt)

Each request is a non-streaming `/v1/chat/completions` call with a bearer key, sent over
loopback keep-alive connections. The upstream returns a fixed completion with usage. The gateway
path includes auth, rate-limit and budget checks, the JSON rewrite, the extra local hop, metrics
and logging (to a discarded handler).

### Latency (one request at a time)

| Path | Median per request | Range over 10 runs | Allocations |
| --- | --- | --- | --- |
| Direct to upstream | 34.7 µs | 33.3–35.7 µs | 115 allocs, ~9.5 KB |
| Through the gateway | 78.9 µs | 78.2–79.6 µs | 304 allocs, ~24 KB |
| **Added by the gateway** | **≈ 44 µs** | | +189 allocs, +14.5 KB |

Against real provider calls, which take hundreds of milliseconds or more, this is well under
0.1 % of the request time.

### Throughput (8 goroutines)

Wall time divided by requests: median 11.8 µs direct and 30.4 µs through the gateway. These runs
use every core, and the load average rose to about 10 while they ran, so the later runs are
slower (direct 10.6–28.1 µs, gateway 26.9–41.2 µs). Treat them as a rough ratio (about 2.6×
more CPU per request than a bare proxy hop), not a capacity figure.

### Not measured

- Streaming responses, the response cache, retries and failover between targets.
- A deployed instance: Cloud Run adds its own front-end and network hops.
