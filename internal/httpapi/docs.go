package httpapi

import (
	_ "embed"
	"io"
	"net/http"
)

//go:embed openapi.json
var openAPISpec []byte

// Swagger UI itself comes from a CDN.
const swaggerPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Last Ticket API</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui.css"></head>
<body><div id="ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url: "/openapi.json", dom_id: "#ui", persistAuthorization: true});</script>
</body></html>`

func (s *server) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(openAPISpec)
}

func (s *server) swaggerUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, swaggerPage)
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "ok")
}

// readiness fails while the database is unreachable, so a load balancer stops
// sending traffic to this instance.
func (s *server) readiness(w http.ResponseWriter, r *http.Request) {
	if s.ready != nil {
		if err := s.ready(r.Context()); err != nil {
			s.log.WarnContext(r.Context(), "not ready", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "not_ready", Message: "database is unreachable"})
			return
		}
	}
	_, _ = io.WriteString(w, "ready")
}
