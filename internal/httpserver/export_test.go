package httpserver

import "net/http"

// RequireAuthForTest exposes the unexported requireAuth middleware to the
// external test package so the gate can be exercised directly by wrapping a stub
// handler - without needing the (not-yet-built) API/dashboard handlers.
func (s *Server) RequireAuthForTest(next http.Handler) http.Handler {
	return s.requireAuth(next)
}
