package gateway

import (
	_ "embed"
	"net/http"
)

// The network dashboard ships inside the gateway binary (AX1 spirit: no
// separate artifact to deploy). It is a single self-contained page that
// reads the public network APIs the gateway already serves.
//
//go:embed dashboard.html
var dashboardHTML []byte

func (s *Server) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(dashboardHTML)
}
