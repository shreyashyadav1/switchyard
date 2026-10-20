package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// BenchmarkOverhead compares a request sent straight to a local fake
// upstream with the same request sent through the gateway. The difference is
// the gateway's added latency per request (auth, limits, JSON rewrite, one
// extra local hop, metrics and logging).
func BenchmarkOverhead(b *testing.B) {
	primary := newUpstream(b, func(w http.ResponseWriter, _ int, _ map[string]any) { completion(w, 100, 20) })
	h := newHarness(b, primary, newUpstream(b, status(500)), "")

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	run := func(b *testing.B, url string) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				req, _ := http.NewRequest("POST", url, strings.NewReader(chatBody))
				req.Header.Set("Authorization", "Bearer "+h.token)
				resp, err := client.Do(req)
				if err != nil {
					b.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					b.Errorf("status %d", resp.StatusCode)
					return
				}
			}
		})
	}
	b.Run("direct", func(b *testing.B) { run(b, primary.URL+"/v1/chat/completions") })
	b.Run("gateway", func(b *testing.B) { run(b, h.srv.URL+"/v1/chat/completions") })
}
