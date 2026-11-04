package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// BenchmarkOverhead compares a request sent straight to a local fake
// upstream with the same request sent through the gateway (auth, limits, JSON
// rewrite, one extra local hop, metrics and logging).
//
// The latency group sends one request at a time, so its ns/op is the time per
// request and gateway minus direct is the latency the gateway adds. The
// throughput group sends from GOMAXPROCS goroutines, so its ns/op is wall time
// divided by requests: the inverse of throughput, not a per-request latency.
func BenchmarkOverhead(b *testing.B) {
	primary := newUpstream(b, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 100, 20) })
	h := newHarness(b, primary, newUpstream(b, status(500)), "")

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	send := func(b *testing.B, url string) bool {
		req, _ := http.NewRequest("POST", url, strings.NewReader(chatBody))
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := client.Do(req)
		if err != nil {
			b.Error(err)
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			b.Errorf("status %d", resp.StatusCode)
			return false
		}
		return true
	}
	serial := func(b *testing.B, url string) {
		b.ReportAllocs()
		for b.Loop() {
			if !send(b, url) {
				return
			}
		}
	}
	parallel := func(b *testing.B, url string) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if !send(b, url) {
					return
				}
			}
		})
	}

	direct := primary.URL + "/v1/chat/completions"
	gateway := h.srv.URL + "/v1/chat/completions"
	b.Run("latency/direct", func(b *testing.B) { serial(b, direct) })
	b.Run("latency/gateway", func(b *testing.B) { serial(b, gateway) })
	b.Run("throughput/direct", func(b *testing.B) { parallel(b, direct) })
	b.Run("throughput/gateway", func(b *testing.B) { parallel(b, gateway) })
}
