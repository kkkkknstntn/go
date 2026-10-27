package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"awesomeProject/internal/auth"
	"awesomeProject/internal/domain"
	"awesomeProject/internal/ids"
)

// authorize checks the bearer token and the role (unless the route is public)
// and puts the principal into the request context.
func (s *server) authorize(a access, next http.HandlerFunc) http.Handler {
	if a.public {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			s.writeError(w, r, fmt.Errorf("%w: send Authorization: Bearer <access token>", domain.ErrUnauthenticated), nil)
			return
		}
		p, err := s.auth.Authenticate(token)
		if err != nil {
			s.writeError(w, r, err, nil)
			return
		}
		if len(a.roles) > 0 && !p.HasRole(a.roles...) {
			s.writeError(w, r, fmt.Errorf("%w: this operation requires role %v", domain.ErrForbidden, a.roles), nil)
			return
		}
		next(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	return token, ok && strings.EqualFold(scheme, "Bearer") && token != ""
}

// principal returns the caller on routes behind authorize.
func principal(r *http.Request) auth.Principal {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		panic("httpapi: principal missing on an authenticated route")
	}
	return p
}

type requestIDKey struct{}

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// withRequestID takes X-Request-ID from the request or generates one, and
// echoes it in the response so a client can quote it when reporting a problem.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID.MatchString(id) {
			id = ids.New("req-")
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug // probes would drown everything else
		}
		s.log.Log(r.Context(), level, "http request",
			"method", r.Method, "path", r.URL.Path, "status", status,
			"duration", time.Since(start).Round(time.Microsecond), "request_id", requestID(r.Context()))
	})
}

// withTimeout limits how long a request may run. Most of that time is spent
// waiting for row locks; when it runs out the query is cancelled and the
// transaction rolled back, so a stuck lock does not pile up requests.
func withTimeout(d time.Duration, next http.Handler) http.Handler {
	if d <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverPanics answers 500 instead of dropping the connection. A transaction
// that was open is rolled back by the deferred Rollback in postgres.InTx.
func (s *server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			s.log.ErrorContext(r.Context(), "panic", "panic", v, "stack", string(debug.Stack()),
				"method", r.Method, "path", r.URL.Path, "request_id", requestID(r.Context()))
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal", Message: "internal error"})
		}()
		next.ServeHTTP(w, r)
	})
}
