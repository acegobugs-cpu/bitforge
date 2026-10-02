// Package api is the runner's HTTP surface: the four routes from plan §3 plus
// /healthz, behind a constant-time X-Internal-Auth check. It owns exactly two
// jobs — turn JSON into runner.Request and turn runner errors into status
// codes — and nothing else; all behaviour lives in runner.Service.
//
// Reading guide:
//
//	Handler()        mux + middleware wiring (read this first)
//	requireAuth      the one security decision in the package
//	create/get/del/list/healthz
//	writeError       the error → status table
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"bitforge/lab-runner/internal/docker"
	"bitforge/lab-runner/internal/runner"
	"bitforge/lab-runner/internal/store"
)

const HeaderInternalAuth = "X-Internal-Auth"

// Engine is the subset of docker.Engine the API needs directly (health).
type Engine interface {
	Ping(ctx context.Context) error
}

type Server struct {
	svc    *runner.Service
	engine Engine
	secret []byte
	log    *slog.Logger
	// CreateTimeout bounds the Docker work for one POST (pull + start).
	CreateTimeout time.Duration
}

func New(svc *runner.Service, eng Engine, secret string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{svc: svc, engine: eng, secret: []byte(secret), log: log, CreateTimeout: 3 * time.Minute}
}

// Handler returns the routed, authenticated http.Handler. Go 1.22 patterns:
// the method is part of the route, so a wrong method is a 405 for free.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz) // unauthenticated: Compose/Kubernetes probes have no secret
	mux.Handle("POST /instances", s.requireAuth(s.create))
	mux.Handle("GET /instances", s.requireAuth(s.list))
	mux.Handle("GET /instances/{id}", s.requireAuth(s.get))
	mux.Handle("DELETE /instances/{id}", s.requireAuth(s.del))
	return s.logRequests(mux)
}

// ---------------------------------------------------------------- middleware

// requireAuth compares the shared secret in constant time. A missing or wrong
// header is 401; an empty configured secret rejects everything (fail closed).
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get(HeaderInternalAuth))
		if len(s.secret) == 0 || subtle.ConstantTimeCompare(got, s.secret) != 1 {
			writeJSON(w, http.StatusUnauthorized, errBody{"unauthorized"})
			return
		}
		next(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rw.status, "ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// ---------------------------------------------------------------- wire types

// createRequest is the JSON body of POST /instances (plan §3). Field names are
// the contract Learn's LabRunnerClient codes against.
type createRequest struct {
	Owner       string            `json:"owner"`
	SessionID   string            `json:"sessionId"`
	UserID      string            `json:"userId"`
	Image       string            `json:"image"`
	ExposedPort int               `json:"exposedPort"`
	TTLSeconds  int               `json:"ttlSeconds"`
	CPUMillis   int               `json:"cpuMillis"`
	MemoryMB    int               `json:"memoryMb"`
	Egress      bool              `json:"egress"`
	ReadOnly    bool              `json:"readOnly"`
	Env         map[string]string `json:"env"`
	Labels      map[string]string `json:"labels"`
	CallbackURL string            `json:"callbackUrl"`
}

// instanceResponse is what every route returns for an instance. It is a
// deliberate subset of store.Instance: no labels map (internal), no UserID.
type instanceResponse struct {
	InstanceID string       `json:"instanceId"`
	SessionID  string       `json:"sessionId"`
	Owner      string       `json:"owner"`
	Status     store.Status `json:"status"`
	Endpoint   string       `json:"endpoint,omitempty"`
	ExpiresAt  time.Time    `json:"expiresAt"`
	FailReason string       `json:"failReason,omitempty"`
}

func toResponse(i store.Instance) instanceResponse {
	return instanceResponse{
		InstanceID: i.ID, SessionID: i.SessionID, Owner: i.Owner, Status: i.Status,
		Endpoint: i.Endpoint, ExpiresAt: i.ExpiresAt, FailReason: i.FailReason,
	}
}

type errBody struct {
	Error string `json:"error"`
}

// ---------------------------------------------------------------- handlers

// create validates the JSON shape (the runner validates the semantics) and
// runs the whole create synchronously: the response is RUNNING or FAILED.
// Pull time is bounded by CreateTimeout; Learn's client uses a long read
// timeout for this one call (plan §6).
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var body createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody{"invalid JSON: " + err.Error()})
		return
	}
	if body.Owner == "" {
		body.Owner = "learn" // the only client in Plan 01; Challenge will set its own
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.CreateTimeout)
	defer cancel()
	inst, err := s.svc.Create(ctx, runner.Request{
		Owner: body.Owner, SessionID: body.SessionID, UserID: body.UserID,
		Image: body.Image, ExposedPort: body.ExposedPort,
		TTL:       time.Duration(body.TTLSeconds) * time.Second,
		CPUMillis: body.CPUMillis, MemoryMB: body.MemoryMB,
		Egress: body.Egress, ReadOnly: body.ReadOnly,
		Env: body.Env, Labels: body.Labels, CallbackURL: body.CallbackURL,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	// A FAILED instance is still a created resource (the client can GET it and
	// read failReason), but the request did not achieve its purpose: 502 tells
	// Learn "Docker said no" without making it parse the body to find out.
	code := http.StatusCreated
	if inst.Status == store.Failed {
		code = http.StatusBadGateway
	}
	writeJSON(w, code, toResponse(inst))
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	inst, ok := s.svc.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody{"instance not found"})
		return
	}
	writeJSON(w, http.StatusOK, toResponse(inst))
}

// del is idempotent: 204 whether the instance was RUNNING, already gone, or
// never existed (plan §3). Only a Docker failure is an error.
func (s *Server) del(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Stop(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// list supports ?label=key=value (plan: ?label=owner=learn). The reserved
// short names owner / sessionId / userId expand to their bitforge.* keys.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	key, value := "", ""
	if raw := r.URL.Query().Get("label"); raw != "" {
		k, v, ok := strings.Cut(raw, "=")
		if !ok || k == "" {
			writeJSON(w, http.StatusBadRequest, errBody{"label must be key=value"})
			return
		}
		key, value = expandLabel(k), v
	}
	items := s.svc.List(key, value)
	out := make([]instanceResponse, 0, len(items))
	for _, i := range items {
		out = append(out, toResponse(i))
	}
	writeJSON(w, http.StatusOK, out)
}

func expandLabel(k string) string {
	switch k {
	case "owner":
		return store.LabelOwner
	case "sessionId":
		return store.LabelSessionID
	case "userId":
		return store.LabelUserID
	}
	return k
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.engine.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody{"docker: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------- errors

// writeError is the single place runner errors become HTTP statuses.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runner.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, errBody{err.Error()})
	case errors.Is(err, runner.ErrNotAllowed):
		writeJSON(w, http.StatusForbidden, errBody{err.Error()})
	case errors.Is(err, runner.ErrNotFound), errors.Is(err, docker.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errBody{err.Error()})
	case errors.Is(err, runner.ErrCapacity):
		writeJSON(w, http.StatusTooManyRequests, errBody{err.Error()})
	case errors.Is(err, runner.ErrDockerFailed), errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusBadGateway, errBody{err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, errBody{fmt.Sprintf("internal: %v", err)})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
