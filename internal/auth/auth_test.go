package auth

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shreyashyadav1/switchyard/internal/config"
)

func TestAuthenticate(t *testing.T) {
	token, digest, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "sy_") || len(digest) != 64 {
		t.Fatalf("token %q digest %q", token, digest)
	}
	keys := []config.Key{{Name: "ci", SHA256: digest, RequestsPerMinute: 10}}

	cases := []struct {
		name      string
		header    string
		anonymous bool
		wantOK    bool
		wantName  string
	}{
		{"valid key", "Bearer " + token, false, true, "ci"},
		{"lowercase scheme", "bearer " + token, false, true, "ci"},
		{"wrong key", "Bearer sy_nope", false, false, ""},
		{"no header", "", false, false, "anonymous"},
		{"no header, anonymous allowed", "", true, true, "anonymous"},
		{"wrong key, anonymous allowed", "Bearer sy_nope", true, false, ""},
		{"basic auth", "Basic Zm9vOmJhcg==", true, true, "anonymous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			id, ok := New(keys, tc.anonymous).Authenticate(r)
			if ok != tc.wantOK || id.Name != tc.wantName {
				t.Fatalf("got (%q, %v), want (%q, %v)", id.Name, ok, tc.wantName, tc.wantOK)
			}
			if tc.wantName == "ci" && id.RequestsPerMinute != 10 {
				t.Errorf("limits not carried: %+v", id)
			}
		})
	}
}
