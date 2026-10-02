package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bitforge/lab-runner/internal/docker"
	"bitforge/lab-runner/internal/runner"
	"bitforge/lab-runner/internal/store"
)

// ---------------------------------------------------------------- harness

const secret = "test-secret"

type rig struct {
	srv *httptest.Server
	eng *docker.FakeEngine
	now time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{eng: docker.NewFakeEngine(), now: time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)}
	st := store.NewStore(func() time.Time { return r.now })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := runner.New(st, r.eng, nil, runner.Limits{
		MaxInstances: 1, MaxTTL: time.Hour, PublicHost: "labs.local",
		Allowlist: docker.ParseAllowlist("ghcr.io/bitforge/labs/"),
	}, log)
	r.srv = httptest.NewServer(New(svc, r.eng, secret, log).Handler())
	t.Cleanup(r.srv.Close)
	return r
}

// do sends an authenticated request and decodes the JSON body (if any).
func (r *rig) do(t *testing.T, method, path string, body any, auth bool) (int, map[string]any) {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, buf)
	if auth {
		req.Header.Set(HeaderInternalAuth, secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

func (r *rig) doList(t *testing.T, path string) (int, []map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+path, nil)
	req.Header.Set(HeaderInternalAuth, secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func validBody(session string) map[string]any {
	return map[string]any{
		"sessionId": session, "userId": "u-1",
		"image": "ghcr.io/bitforge/labs/hello-flag:1", "exposedPort": 8080,
		"ttlSeconds": 1800, "env": map[string]string{"FLAG_1": "FLAG{x}"},
		"callbackUrl": "http://learn/internal/" + session,
	}
}

// ---------------------------------------------------------------- auth

func TestAuthRequiredOnEveryInstanceRoute(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/instances"}, {"GET", "/instances"}, {"GET", "/instances/x"}, {"DELETE", "/instances/x"},
	} {
		if code, _ := r.do(t, c.method, c.path, validBody("s"), false); code != http.StatusUnauthorized {
			t.Errorf("%s %s without secret = %d, want 401", c.method, c.path, code)
		}
	}
	// wrong secret, same length — constant-time compare must still reject
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/instances", nil)
	req.Header.Set(HeaderInternalAuth, strings.Repeat("x", len(secret)))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong secret = %d", resp.StatusCode)
	}
	// healthz needs no secret
	if code, _ := r.do(t, "GET", "/healthz", nil, false); code != http.StatusOK {
		t.Errorf("healthz = %d, want 200", code)
	}
	if len(r.eng.Created) != 0 {
		t.Error("unauthenticated POST must never reach the engine")
	}
}

func TestEmptySecretFailsClosed(t *testing.T) {
	st := store.NewStore(nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := runner.New(st, docker.NewFakeEngine(), nil, runner.Limits{MaxInstances: 1, MaxTTL: time.Hour}, log)
	srv := httptest.NewServer(New(svc, docker.NewFakeEngine(), "", log).Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/instances", nil)
	req.Header.Set(HeaderInternalAuth, "")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("empty configured secret must reject even an empty header: %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------- lifecycle

func TestCreateGetListDelete(t *testing.T) {
	r := newRig(t)

	code, body := r.do(t, "POST", "/instances", validBody("s1"), true)
	if code != http.StatusCreated {
		t.Fatalf("POST = %d %v", code, body)
	}
	id, _ := body["instanceId"].(string)
	if id == "" || body["status"] != "RUNNING" || body["endpoint"] != "labs.local:32768" || body["owner"] != "learn" {
		t.Errorf("POST body = %v", body)
	}
	if _, leaked := body["labels"]; leaked {
		t.Error("labels are internal and must not be in the response")
	}
	if exp, _ := body["expiresAt"].(string); !strings.HasPrefix(exp, "2026-10-02T12:30:00") {
		t.Errorf("expiresAt = %v, want now+1800s", body["expiresAt"])
	}

	code, got := r.do(t, "GET", "/instances/"+id, nil, true)
	if code != http.StatusOK || got["instanceId"] != id {
		t.Errorf("GET = %d %v", code, got)
	}

	code, list := r.doList(t, "/instances?label=owner=learn")
	if code != http.StatusOK || len(list) != 1 || list[0]["instanceId"] != id {
		t.Errorf("list owner=learn = %d %v", code, list)
	}
	if _, list = r.doList(t, "/instances?label=owner=challenge"); len(list) != 0 {
		t.Errorf("list owner=challenge must be empty, got %v", list)
	}
	if _, list = r.doList(t, "/instances?label=sessionId=s1"); len(list) != 1 {
		t.Errorf("list by sessionId short name failed: %v", list)
	}
	if code, _ = r.doList(t, "/instances?label=garbage"); code != http.StatusBadRequest {
		t.Errorf("malformed label filter = %d, want 400", code)
	}

	if code, _ = r.do(t, "DELETE", "/instances/"+id, nil, true); code != http.StatusNoContent {
		t.Errorf("DELETE = %d, want 204", code)
	}
	if code, _ = r.do(t, "DELETE", "/instances/"+id, nil, true); code != http.StatusNoContent {
		t.Errorf("second DELETE = %d, want 204 (idempotent)", code)
	}
	if code, _ = r.do(t, "DELETE", "/instances/never", nil, true); code != http.StatusNoContent {
		t.Errorf("DELETE unknown = %d, want 204 (idempotent)", code)
	}
	if _, got = r.do(t, "GET", "/instances/"+id, nil, true); got["status"] != "STOPPED" {
		t.Errorf("after DELETE status = %v", got["status"])
	}
	if code, _ = r.do(t, "GET", "/instances/nope", nil, true); code != http.StatusNotFound {
		t.Errorf("GET unknown = %d, want 404", code)
	}
}

// ---------------------------------------------------------------- error table

func TestErrorStatuses(t *testing.T) {
	r := newRig(t)

	// 400: malformed JSON, unknown field, semantic validation
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/instances", strings.NewReader("{not json"))
	req.Header.Set(HeaderInternalAuth, secret)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad JSON = %d", resp.StatusCode)
	}
	b := validBody("s1")
	b["bogusField"] = 1
	if code, _ := r.do(t, "POST", "/instances", b, true); code != http.StatusBadRequest {
		t.Errorf("unknown field = %d, want 400 (DisallowUnknownFields)", code)
	}
	b = validBody("s1")
	b["exposedPort"] = 0
	if code, body := r.do(t, "POST", "/instances", b, true); code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "exposedPort") {
		t.Errorf("port 0 = %d %v", code, body)
	}

	// 403: allowlist
	b = validBody("s1")
	b["image"] = "alpine"
	if code, _ := r.do(t, "POST", "/instances", b, true); code != http.StatusForbidden {
		t.Errorf("image not allowed = %d, want 403", code)
	}

	// 429: capacity (MaxInstances = 1)
	if code, _ := r.do(t, "POST", "/instances", validBody("s1"), true); code != http.StatusCreated {
		t.Fatalf("first create = %d", code)
	}
	if code, _ := r.do(t, "POST", "/instances", validBody("s2"), true); code != http.StatusTooManyRequests {
		t.Errorf("over capacity = %d, want 429", code)
	}

	// 502: Docker failed → FAILED instance in the body, still GET-able
	r.do(t, "DELETE", "/instances/fake-1", nil, true)
	r.eng.FailCreate = errors.New("pull: manifest unknown")
	code, body := r.do(t, "POST", "/instances", validBody("s3"), true)
	if code != http.StatusBadGateway || body["status"] != "FAILED" || !strings.Contains(body["failReason"].(string), "manifest unknown") {
		t.Errorf("docker failure = %d %v", code, body)
	}
	if id, _ := body["instanceId"].(string); id != "" {
		if c, got := r.do(t, "GET", "/instances/"+id, nil, true); c != http.StatusOK || got["status"] != "FAILED" {
			t.Errorf("FAILED instance must remain queryable: %d %v", c, got)
		}
	}

	// 502 on DELETE when Docker cannot remove
	r.eng.FailCreate = nil
	r.do(t, "POST", "/instances", validBody("s4"), true)
	r.eng.FailRemove = errors.New("daemon busy")
	if code, _ := r.do(t, "DELETE", "/instances/fake-2", nil, true); code != http.StatusBadGateway {
		t.Errorf("remove failure = %d, want 502", code)
	}

	// 405 for free from the method-aware mux
	if code, _ := r.do(t, "PUT", "/instances", validBody("s9"), true); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT = %d, want 405", code)
	}
}

func TestHealthzReflectsDocker(t *testing.T) {
	r := newRig(t)
	r.eng.FailPing = errors.New("cannot connect to the Docker daemon")
	if code, body := r.do(t, "GET", "/healthz", nil, false); code != http.StatusServiceUnavailable || !strings.Contains(body["error"].(string), "Docker") {
		t.Errorf("healthz with dead daemon = %d %v", code, body)
	}
}

func TestCreateTimeoutIs502(t *testing.T) {
	r := newRig(t)
	// swap in a server with a tiny timeout and an engine that hangs
	st := store.NewStore(func() time.Time { return r.now })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hang := &hangingEngine{FakeEngine: docker.NewFakeEngine()}
	svc := runner.New(st, hang, nil, runner.Limits{MaxInstances: 1, MaxTTL: time.Hour, Allowlist: docker.ParseAllowlist("ghcr.io/")}, log)
	s := New(svc, hang, secret, log)
	s.CreateTimeout = 50 * time.Millisecond
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	b, _ := json.Marshal(validBody("s1"))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/instances", bytes.NewReader(b))
	req.Header.Set(HeaderInternalAuth, secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	// the runner reports the timeout as a FAILED instance (Docker never answered)
	if resp.StatusCode != http.StatusBadGateway || body["status"] != "FAILED" {
		t.Errorf("timeout = %d %v", resp.StatusCode, body)
	}
}

// hangingEngine blocks Create until the context is done.
type hangingEngine struct{ *docker.FakeEngine }

func (h *hangingEngine) Create(ctx context.Context, _ docker.CreateSpec) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}
