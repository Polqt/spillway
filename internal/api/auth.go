package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

const authFailedMessage = `Invalid API key. Provide a valid key in the Authorization header as "Bearer <key>".`

// authenticator checks local keys. Keys are stored only as SHA-256 digests,
// so every comparison is over equal-length values and the key length cannot
// leak through timing.
type authenticator struct {
	names     []string
	digests   [][sha256.Size]byte
	dashboard []bool
}

func newAuthenticator(keys []LocalKey) (*authenticator, error) {
	if len(keys) == 0 {
		return nil, errors.New("api: at least one local key is required (ADR 0003)")
	}
	a := &authenticator{}
	for _, k := range keys {
		if k.Key.IsZero() {
			return nil, errors.New("api: local key " + k.Name + " is empty")
		}
		a.names = append(a.names, k.Name)
		a.digests = append(a.digests, sha256.Sum256([]byte(k.Key.Reveal())))
		a.dashboard = append(a.dashboard, k.Dashboard)
	}
	return a, nil
}

// match returns the client app and dashboard permission for token.
// It compares against every key with no early exit and selects the match
// without branching, so timing reveals neither whether nor which key matched.
func (a *authenticator) match(token string) (string, bool, bool) {
	presented := sha256.Sum256([]byte(token))
	idx, matched, dashboard := 0, 0, 0
	for i, d := range a.digests {
		eq := subtle.ConstantTimeCompare(presented[:], d[:])
		idx = subtle.ConstantTimeSelect(eq, i, idx)
		dashboard = subtle.ConstantTimeSelect(eq, boolInt(a.dashboard[i]), dashboard)
		matched |= eq
	}
	if matched != 1 {
		return "", false, false
	}
	return a.names[idx], dashboard == 1, true
}

// middleware requires "Authorization: Bearer <local key>" on API paths and
// records the client app for the access log. The static shell carries no data.
func (a *authenticator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/dashboard" || r.URL.Path == "/dashboard.css" || r.URL.Path == "/dashboard.js" {
			next.ServeHTTP(w, r)
			return
		}
		scheme, token, hasToken := strings.Cut(r.Header.Get("Authorization"), " ")
		name, dashboard, ok := a.match(token)
		if !hasToken || !strings.EqualFold(scheme, "Bearer") || !ok {
			writeError(w, http.StatusUnauthorized, authFailedMessage, typeAuthentication, "invalid_api_key")
			return
		}
		recordFrom(r.Context()).clientApp = name
		if strings.HasPrefix(r.URL.Path, "/api") && !dashboard {
			writeError(w, http.StatusForbidden, "This API key does not have dashboard access.", "permission_denied", "permission_denied")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
