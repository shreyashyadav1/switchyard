// Package auth checks client API keys against stored SHA-256 digests.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/shreyashyadav1/switchyard/internal/config"
)

// Identity is the authenticated caller and its limits. Zero limits mean
// unlimited.
type Identity struct {
	Name              string
	RequestsPerMinute int
	DailyBudgetUSD    float64
}

// Anonymous is the identity used when anonymous access is allowed.
var Anonymous = Identity{Name: "anonymous"}

type entry struct {
	digest []byte
	id     Identity
}

// Authenticator maps bearer tokens to identities.
type Authenticator struct {
	entries   []entry
	anonymous bool
}

// New builds an Authenticator from validated config keys.
func New(keys []config.Key, allowAnonymous bool) *Authenticator {
	a := &Authenticator{anonymous: allowAnonymous}
	for _, k := range keys {
		d, _ := hex.DecodeString(k.SHA256)
		a.entries = append(a.entries, entry{
			digest: d,
			id:     Identity{Name: k.Name, RequestsPerMinute: k.RequestsPerMinute, DailyBudgetUSD: k.DailyBudgetUSD},
		})
	}
	return a
}

// Authenticate returns the caller's identity, or false if the request has no
// valid key and anonymous access is off. A request that presents a key must
// present a valid one, even when anonymous access is on.
func (a *Authenticator) Authenticate(r *http.Request) (Identity, bool) {
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		return Anonymous, a.anonymous
	}
	sum := sha256.Sum256([]byte(token))
	var found Identity
	matched := 0
	// Compare against every entry so timing doesn't reveal which one matched.
	for _, e := range a.entries {
		if subtle.ConstantTimeCompare(sum[:], e.digest) == 1 {
			found = e.id
			matched = 1
		}
	}
	return found, matched == 1
}

func bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

// NewToken returns a random client token and its SHA-256 digest for config.
func NewToken() (token, digest string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = "sy_" + base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}
