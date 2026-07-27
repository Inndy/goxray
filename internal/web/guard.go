package web

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

// The dashboard exposes every recorded conversation verbatim over HTTP. Binding
// loopback is not sufficient protection on its own, because a web page the user
// visits can also reach loopback:
//
//   - A plain cross-origin fetch cannot read our responses (we never send CORS
//     headers), but DNS rebinding defeats that — a hostile domain re-resolved to
//     127.0.0.1 looks same-origin to the browser. What a rebound request cannot
//     forge is the Host header, which still carries the attacker's domain. So
//     requiring a loopback Host is the actual defense.
//   - The Origin check is secondary and rejects remote pages outright. Loopback
//     origins are allowed so a dev server on another port still works; code
//     already running on the machine is not the threat this guards against.
//
// Nothing here is a substitute for auth. Exposing this port off-loopback would
// require real authentication first.
func guard(uiPort int, next http.Handler) http.Handler {
	port := strconv.Itoa(uiPort)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, port) {
			deny(w, "host not allowed")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !originAllowed(o) {
			deny(w, "origin not allowed")
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func deny(w http.ResponseWriter, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// hostAllowed requires a loopback hostname, and when the Host carries a port it
// must be the port we are actually serving.
func hostAllowed(host, port string) bool {
	if host == "" {
		return false
	}
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		// No port present.
		return isLoopbackHost(host)
	}
	return isLoopbackHost(h) && p == port
}

func originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

// recoverer keeps a dashboard bug from surfacing as a dropped connection. The
// proxy's fail-closed path is unaffected either way — this server shares only
// read-only access to the store — but an explicit JSON 500 is easier to debug
// than net/http's bare connection teardown.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Printf("goxray: dashboard panic on %s %s: %v", r.Method, r.URL.Path, v)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
