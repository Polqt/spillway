package api

import (
	"net"
	"net/http"
	"slices"
	"strings"
)

// LoopbackHosts returns the Host names a loopback listener accepts:
// localhost, 127.0.0.1, ::1 and listenHost, lowercased and deduplicated.
func LoopbackHosts(listenHost string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h := strings.ToLower(listenHost); !slices.Contains(hosts, h) {
		hosts = append(hosts, h)
	}
	return hosts
}

const hostNotAllowed = "Host header not allowed. Spillway listens on loopback and accepts only localhost, 127.0.0.1, [::1] or its listen address; use one of these in the base URL."

// hostCheck refuses requests whose Host is not in allowed, so a DNS
// rebinding page cannot reach a loopback listener (ADR 0003). The port is
// not compared: a rebinding page controls the name, not the port. The
// refusal never echoes the presented Host.
func hostCheck(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(allowed, requestHost(r.Host)) {
			writeError(w, http.StatusForbidden, hostNotAllowed, typeInvalidRequest, "host_not_allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestHost strips the port and IPv6 brackets from a Host header and
// lowercases it. Anything malformed comes back unchanged and matches nothing.
func requestHost(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.HasPrefix(hostport, "[") && strings.HasSuffix(hostport, "]") {
		host = hostport[1 : len(hostport)-1]
	}
	return strings.ToLower(host)
}
