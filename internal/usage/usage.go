// Package usage reads token counts from OpenAI-style responses and prices them.
package usage

import (
	"bytes"
	"encoding/json"

	"github.com/shreyashyadav1/switchyard/internal/config"
)

// Usage is the token count of one response.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

type wire struct {
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// Parse reads the top-level "usage" object of a JSON response body.
func Parse(body []byte) (Usage, bool) {
	var w wire
	if err := json.Unmarshal(body, &w); err != nil || w.Usage == nil {
		return Usage{}, false
	}
	return Usage{InputTokens: w.Usage.PromptTokens, OutputTokens: w.Usage.CompletionTokens}, true
}

// ParseSSELine reads usage from one server-sent-events line such as
// `data: {...,"usage":{...}}`. Providers send usage in the final chunk when
// the request sets stream_options.include_usage.
func ParseSSELine(line []byte) (Usage, bool) {
	payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return Usage{}, false
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || payload[0] != '{' || !bytes.Contains(payload, []byte(`"usage"`)) {
		return Usage{}, false
	}
	return Parse(payload)
}

// Cost prices u at p, in US dollars.
func Cost(p config.Price, u Usage) float64 {
	return (float64(u.InputTokens)*p.Input + float64(u.OutputTokens)*p.Output) / 1e6
}
