package browse

import (
	"net/http"
	"strings"
	"time"
)

// withSecurity sets the security headers on every response (R3-Q6):
// CSP default-src 'self' tuned so client-side render works (script-src 'self',
// style-src 'self', no unsafe-inline), nosniff, no-referrer, frame DENY.
func (h *Handler) withSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'self'; form-action 'self'")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// withHostAllowlist rejects requests whose Host header is not in the
// config-driven allowlist (git.cmposer.cc + EIP + localhost, R8-Q5). Mismatch
// -> 400 so the UI is only reachable via the canonical hostnames.
func (h *Handler) withHostAllowlist(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.hostAllowlist[normalizeHost(r.Host)] {
			http.Error(w, "unknown host", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAudit logs one slog Info line per request (R4-Q9): client cert CN,
// client IP, method, path, status, duration. Query strings and bodies are
// never logged.
func (h *Handler) withAudit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		h.log.Info("browse request",
			"cn", clientCN(r),
			"ip", clientIP(r),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

// statusRecorder captures the response status for audit logging without
// wrapping the body writer (pass-through).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader implements http.ResponseWriter, capturing the status code.
func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// clientIP extracts the remote address without the port.
func clientIP(r *http.Request) string {
	host, _, err := splitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clientCN returns the verified client certificate CN (R4-Q9). With
// RequireAndVerifyClientCert the peer cert is always present; a missing one
// yields "unknown" defensively.
func clientCN(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "unknown"
	}
	return r.TLS.PeerCertificates[0].Subject.CommonName
}

// splitHostPort splits "host:port", tolerating IPv6 "[::1]:443".
func splitHostPort(addr string) (string, string, error) {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if strings.HasPrefix(addr, "[") {
			if j := strings.Index(addr, "]:"); j >= 0 {
				return addr[1:j], addr[j+2:], nil
			}
		}
		return addr[:i], addr[i+1:], nil
	}
	return addr, "", nil
}
