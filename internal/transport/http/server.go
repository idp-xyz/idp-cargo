// Package http is Cargo's inbound HTTP server: routing on the standard library's ServeMux and the middleware every
// request passes, outermost first: a correlation id (X-Correlation-ID, taken when well-formed, else generated),
// recovery from a panic as a 500 problem, an access log, and authentication of the bearer token into the principal
// the contexts' adapters and authorizers read. The contexts register their routes (RegisterHTTP); /healthz and
// /readyz need no token. Errors are RFC 9457 problems (foundation/httpapi).
package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	nethttp "net/http"
	"regexp"
	"strings"
	"time"

	"go.idp.xyz/idp-cargo/internal/foundation/httpapi"
	"go.idp.xyz/idp-cargo/internal/foundation/identity"
)

// Registrar is a context that registers its routes.
type Registrar interface {
	RegisterHTTP(mux *nethttp.ServeMux)
}

// Config is what the handler needs: the token verifier, a logger, and Ready for /readyz (the database answering).
type Config struct {
	Verifier identity.Verifier
	Logger   *slog.Logger
	Ready    func(context.Context) error
}

// NewHandler builds the handler with every registrar's routes.
func NewHandler(cfg Config, registrars ...Registrar) (nethttp.Handler, error) {
	if cfg.Verifier == nil || cfg.Logger == nil || cfg.Ready == nil {
		return nil, errors.New("http: the handler needs a verifier, a logger and a readiness check")
	}
	api := nethttp.NewServeMux()
	for _, r := range registrars {
		r.RegisterHTTP(api)
	}
	open := nethttp.NewServeMux()
	open.HandleFunc("GET /healthz", func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		httpapi.WriteJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
	})
	open.HandleFunc("GET /readyz", func(w nethttp.ResponseWriter, r *nethttp.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := cfg.Ready(ctx); err != nil {
			cfg.Logger.WarnContext(r.Context(), "not ready", "error", err)
			httpapi.WriteProblem(w, r, nethttp.StatusServiceUnavailable, "UNAVAILABLE", "")
			return
		}
		httpapi.WriteJSON(w, nethttp.StatusOK, map[string]string{"status": "ready"})
	})
	open.Handle("/v1/", authenticate(cfg.Verifier, routed(api)))
	open.Handle("/", routed(nethttp.NewServeMux()))
	return correlate(logRequests(cfg.Logger, recoverPanic(cfg.Logger, open))), nil
}

var correlationPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,64}$`)

// correlate takes a well-formed X-Correlation-ID or makes one, puts it on the context and echoes it.
func correlate(next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		id := r.Header.Get("X-Correlation-ID")
		if !correlationPattern.MatchString(id) {
			var b [16]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Correlation-ID", id)
		next.ServeHTTP(w, r.WithContext(httpapi.WithCorrelationID(r.Context(), id)))
	})
}

// status remembers the status a handler wrote.
type status struct {
	nethttp.ResponseWriter
	code    int
	written bool
}

func (s *status) WriteHeader(code int) {
	if !s.written {
		s.code, s.written = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *status) Write(b []byte) (int, error) {
	if !s.written {
		s.code, s.written = nethttp.StatusOK, true
	}
	return s.ResponseWriter.Write(b)
}

func (s *status) Unwrap() nethttp.ResponseWriter { return s.ResponseWriter }

// Flush passes a flush through to the connection (a handler streaming its response).
func (s *status) Flush() {
	if !s.written {
		s.code, s.written = nethttp.StatusOK, true
	}
	_ = nethttp.NewResponseController(s.ResponseWriter).Flush()
}

// recoverPanic turns a panic into a logged 500 problem. When the handler had written already, the connection is
// aborted instead, so that the client sees a broken response, never a truncated one that looks complete.
func recoverPanic(logger *slog.Logger, next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		sw := &status{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v != nethttp.ErrAbortHandler {
				logger.ErrorContext(r.Context(), "panic", "correlation_id", httpapi.CorrelationID(r.Context()), "path", r.URL.Path, "panic", v)
			}
			if v == nethttp.ErrAbortHandler || sw.written {
				panic(nethttp.ErrAbortHandler)
			}
			httpapi.WriteProblem(sw, r, nethttp.StatusInternalServerError, "INTERNAL", "")
		}()
		next.ServeHTTP(sw, r)
	})
}

// logRequests logs each request: method, route, status, duration, correlation id and the principal's tenant and
// actor (ids only; never bodies, tokens or query strings).
func logRequests(logger *slog.Logger, next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		start := time.Now()
		sw := &status{ResponseWriter: w}
		var who identity.Principal
		done := false
		defer func() { // also when the response is aborted
			code := sw.code
			switch {
			case !done:
				code = 0 // aborted: no complete response
			case !sw.written:
				code = nethttp.StatusOK
			}
			logger.InfoContext(r.Context(), "request", "method", r.Method, "path", r.URL.Path, "status", code, "duration_ms",
				time.Since(start).Milliseconds(), "correlation_id", httpapi.CorrelationID(r.Context()), "tenant_id", who.TenantID, "actor_id", who.ActorID)
		}()
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), seenKey{}, &who)))
		done = true
	})
}

type seenKey struct{}

// authenticate verifies the bearer token into the principal on the context (401 otherwise).
func authenticate(v identity.Verifier, next nethttp.Handler) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			httpapi.WriteProblem(w, r, nethttp.StatusUnauthorized, "UNAUTHENTICATED", "send a bearer token")
			return
		}
		p, err := v.Verify(r.Context(), strings.TrimSpace(token))
		if err != nil {
			httpapi.WriteProblem(w, r, nethttp.StatusUnauthorized, "UNAUTHENTICATED", "the bearer token is not valid")
			return
		}
		if seen, ok := r.Context().Value(seenKey{}).(*identity.Principal); ok {
			*seen = p
		}
		next.ServeHTTP(w, r.WithContext(identity.WithPrincipal(r.Context(), p)))
	})
}

// routed answers a request no route takes, or takes with another method, with a problem (404 or 405 with Allow)
// instead of the mux's plain text.
func routed(mux *nethttp.ServeMux) nethttp.Handler {
	return nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		probe := &headerOnly{header: nethttp.Header{}}
		h, _ := mux.Handler(r)
		h.ServeHTTP(probe, r)
		if probe.code == nethttp.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			httpapi.WriteProblem(w, r, nethttp.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "")
			return
		}
		httpapi.WriteProblem(w, r, nethttp.StatusNotFound, "NOT_FOUND", "no such resource")
	})
}

// headerOnly records what the mux's not-found or method-not-allowed handler writes, and drops its body.
type headerOnly struct {
	header nethttp.Header
	code   int
}

func (h *headerOnly) Header() nethttp.Header      { return h.header }
func (h *headerOnly) Write(b []byte) (int, error) { return len(b), nil }
func (h *headerOnly) WriteHeader(code int)        { h.code = code }

// Server is an http.Server with Cargo's timeouts. Requests run under base: cancelling it cancels the requests in
// flight (and their database work), as a shutdown that ran out of time does.
func Server(addr string, h nethttp.Handler, logger *slog.Logger, base context.Context) *nethttp.Server {
	return &nethttp.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext: func(net.Listener) context.Context { return base }}
}
