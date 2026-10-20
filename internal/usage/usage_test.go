package usage

import (
	"math"
	"testing"

	"github.com/shreyashyadav1/switchyard/internal/config"
)

func TestParse(t *testing.T) {
	u, ok := Parse([]byte(`{"id":"x","usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150}}`))
	if !ok || u.InputTokens != 120 || u.OutputTokens != 30 {
		t.Fatalf("got %+v %v", u, ok)
	}
	if _, ok := Parse([]byte(`{"id":"x"}`)); ok {
		t.Error("missing usage should not parse")
	}
	if _, ok := Parse([]byte(`not json`)); ok {
		t.Error("invalid JSON should not parse")
	}
}

func TestParseSSELine(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
		in   int64
	}{
		{`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}` + "\n", true, 7},
		{`data:{"usage":{"prompt_tokens":9,"completion_tokens":1}}`, true, 9},
		{`data: {"choices":[{"delta":{"content":"hi"}}]}`, false, 0},
		{`data: {"choices":[],"usage":null}`, false, 0},
		{`data: [DONE]`, false, 0},
		{`: keep-alive`, false, 0},
		{``, false, 0},
	}
	for _, tc := range cases {
		u, ok := ParseSSELine([]byte(tc.line))
		if ok != tc.ok || u.InputTokens != tc.in {
			t.Errorf("%q: got %+v %v", tc.line, u, ok)
		}
	}
}

func TestCost(t *testing.T) {
	got := Cost(config.Price{Input: 0.15, Output: 0.60}, Usage{InputTokens: 1_000_000, OutputTokens: 500_000})
	if math.Abs(got-0.45) > 1e-12 {
		t.Errorf("cost = %v, want 0.45", got)
	}
}
