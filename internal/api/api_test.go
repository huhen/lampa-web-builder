package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/huhen/lampa-web-builder/internal/builder"
	"github.com/huhen/lampa-web-builder/internal/config"
	"github.com/huhen/lampa-web-builder/internal/state"
	"github.com/huhen/lampa-web-builder/internal/testutil"
)

type stubUpstream struct {
	head    string
	headErr error
}

func (s *stubUpstream) LsRemoteHead(context.Context) (string, error) {
	return s.head, s.headErr
}
func (s *stubUpstream) EnsureCopy(context.Context, string) error { return nil }
func (s *stubUpstream) Fetch(context.Context, string) error      { return nil }

type stubRunner struct{ gate chan struct{} }

func (r *stubRunner) Run(ctx context.Context, _, _, domain, archivePath string, _ io.Writer) error {
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return os.WriteFile(archivePath, []byte("archive of "+domain), 0o644)
}

type fixture struct {
	handler http.Handler
	git     *stubUpstream
	run     *stubRunner
}

func newFixture(t *testing.T, startWorker bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Listen: ":0", APIKey: "secret-key", DataDir: dir, AssetsDir: dir,
		CacheSize: 2, DefaultDomain: "test.example",
	}
	git := &stubUpstream{}
	run := &stubRunner{}
	b := builder.New(cfg, store, git, run)
	if startWorker {
		ctx, cancel := context.WithCancel(context.Background())
		go b.RunWorker(ctx)
		t.Cleanup(cancel)
	}
	return &fixture{handler: New(b, "test-version"), git: git, run: run}
}

func (f *fixture) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("X-API-Key", "secret-key")
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	f := newFixture(t, false)
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAuthRequired(t *testing.T) {
	f := newFixture(t, false)
	for _, path := range []string{"/api/v1/status", "/api/v1/builds", "/api/v1/builds/x"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without key = %d, want 401", path, rec.Code)
		}
		// Wrong key is rejected too.
		req = httptest.NewRequest("GET", path, nil)
		req.Header.Set("X-API-Key", "wrong")
		rec = httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with wrong key = %d, want 401", path, rec.Code)
		}
	}
}

func TestStatusShape(t *testing.T) {
	f := newFixture(t, false)
	code, body := f.do(t, "GET", "/api/v1/status", "")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "available_commit", "latest_seen_commit", "bad_commit", "poll"} {
		if _, ok := got[k]; !ok {
			t.Errorf("status missing %q: %s", k, body)
		}
	}
	if got["version"] != "test-version" {
		t.Errorf("version = %v", got["version"])
	}
}

func TestCreateBuildValidation(t *testing.T) {
	f := newFixture(t, false)
	if code, _ := f.do(t, "POST", "/api/v1/builds", "{not json"); code != 400 {
		t.Errorf("bad json = %d, want 400", code)
	}
	for _, d := range []string{`"UPPER.example"`, `"a:80"`, `"a/b"`, `""`} {
		if code, _ := f.do(t, "POST", "/api/v1/builds", `{"domain":`+d+`}`); code != 400 {
			t.Errorf("domain %s = %d, want 400", d, code)
		}
	}
	// No available commit yet -> 503.
	if code, body := f.do(t, "POST", "/api/v1/builds", `{"domain":"d.example"}`); code != 503 || !strings.Contains(string(body), "no_available_commit") {
		t.Errorf("no commit = %d %s, want 503 no_available_commit", code, body)
	}
}

func TestCheckAndCreateAndDownload(t *testing.T) {
	f := newFixture(t, true)
	f.git.head = "1111111111111111111111111111111111111111"

	code, body := f.do(t, "POST", "/api/v1/check", "")
	if code != 202 || !strings.Contains(string(body), "true") {
		t.Fatalf("check = %d %s, want 202 triggered", code, body)
	}

	// While the test build is pending/running, orders are rejected with 409.
	// It may already be done; either way accept 202 afterwards.
	testutil.WaitFor(t, func() bool {
		code, _ := f.do(t, "POST", "/api/v1/builds", `{"domain":"probe.example"}`)
		return code == 202 || code == 409
	}, "orders accepted or rejected during test build")

	// Wait for the commit to become available, then order a real build.
	testutil.WaitFor(t, func() bool {
		_, body := f.do(t, "GET", "/api/v1/status", "")
		return strings.Contains(string(body), `"available_commit":"1111`)
	}, "commit available")
	code, body = f.do(t, "POST", "/api/v1/builds", `{"domain":"d.example"}`)
	if code != 202 {
		t.Fatalf("order = %d %s", code, body)
	}
	var created struct {
		BuildID string `json:"build_id"`
		Cached  bool   `json:"cached"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.BuildID == "" {
		t.Fatalf("create body = %s (%v)", body, err)
	}

	// Poll the build status until success.
	testutil.WaitFor(t, func() bool {
		_, body := f.do(t, "GET", "/api/v1/builds/"+created.BuildID, "")
		return strings.Contains(string(body), `"status":"success"`)
	}, "ordered build success")

	// Archive is downloadable; logs are plain text.
	code, arch := f.do(t, "GET", "/api/v1/builds/"+created.BuildID+"/archive", "")
	if code != 200 || !bytes.HasPrefix(arch, []byte("archive of ")) {
		t.Errorf("archive = %d %q", code, arch)
	}
	code, logs := f.do(t, "GET", "/api/v1/builds/"+created.BuildID+"/logs", "")
	if code != 200 || !strings.Contains(string(logs), created.BuildID) {
		t.Errorf("logs = %d %q", code, logs)
	}

	// Same domain again -> cache hit, same id.
	code, body = f.do(t, "POST", "/api/v1/builds", `{"domain":"d.example"}`)
	var again struct {
		BuildID string `json:"build_id"`
		Cached  bool   `json:"cached"`
	}
	json.Unmarshal(body, &again)
	if code != 202 || !again.Cached || again.BuildID != created.BuildID {
		t.Errorf("second order = %d %s, want cached same id", code, body)
	}

	// Unknown build id -> 404 on all three endpoints.
	for _, p := range []string{"/api/v1/builds/b-nope", "/api/v1/builds/b-nope/archive", "/api/v1/builds/b-nope/logs"} {
		if code, _ := f.do(t, "GET", p, ""); code != 404 {
			t.Errorf("%s = %d, want 404", p, code)
		}
	}
}

func TestCheckBusyConflict(t *testing.T) {
	f := newFixture(t, false) // no worker: pending test build stays pending
	f.git.head = "1111111111111111111111111111111111111111"
	if code, _ := f.do(t, "POST", "/api/v1/check", ""); code != 202 {
		t.Fatalf("first check = %d", code)
	}
	code, body := f.do(t, "POST", "/api/v1/check", "")
	if code != 409 || !strings.Contains(string(body), "busy") {
		t.Errorf("second check = %d %s, want 409 busy", code, body)
	}
	code, body = f.do(t, "POST", "/api/v1/builds", `{"domain":"d.example"}`)
	if code != 409 || !strings.Contains(string(body), "test_build_in_progress") {
		t.Errorf("order = %d %s, want 409 test_build_in_progress", code, body)
	}
}

func TestQueueFull(t *testing.T) {
	f := newFixture(t, true)
	f.git.head = "1111111111111111111111111111111111111111"
	f.do(t, "POST", "/api/v1/check", "")
	testutil.WaitFor(t, func() bool {
		_, b := f.do(t, "GET", "/api/v1/status", "")
		return strings.Contains(string(b), `"available_commit":"1111`)
	}, "commit available")

	f.run.gate = make(chan struct{})
	// d1 first, then wait until the worker is stuck in the gate: with the
	// runner busy the queue (capacity CacheSize+1 = 3) deterministically
	// accepts d2-d4 and rejects d5.
	var d1 struct {
		BuildID string `json:"build_id"`
	}
	testutil.WaitFor(t, func() bool {
		code, body := f.do(t, "POST", "/api/v1/builds", `{"domain":"d1.example"}`)
		if code != 202 {
			return false // the finished test build may still release the runner
		}
		if err := json.Unmarshal(body, &d1); err != nil || d1.BuildID == "" {
			t.Fatalf("d1 body = %s (%v)", body, err)
		}
		return true
	}, "d1 accepted")
	ids := []string{d1.BuildID}
	testutil.WaitFor(t, func() bool {
		_, b := f.do(t, "GET", "/api/v1/builds/"+d1.BuildID, "")
		return strings.Contains(string(b), `"status":"running"`)
	}, "first build running")

	for _, d := range []string{"d2.example", "d3.example", "d4.example"} {
		code, body := f.do(t, "POST", "/api/v1/builds", `{"domain":"`+d+`"}`)
		if code != 202 {
			t.Fatalf("%s = %d %s", d, code, body)
		}
		var created struct {
			BuildID string `json:"build_id"`
		}
		if err := json.Unmarshal(body, &created); err != nil || created.BuildID == "" {
			t.Fatalf("%s body = %s (%v)", d, body, err)
		}
		ids = append(ids, created.BuildID)
	}
	if code, body := f.do(t, "POST", "/api/v1/builds", `{"domain":"d5.example"}`); code != 429 || !strings.Contains(string(body), "queue_full") {
		t.Errorf("d5 = %d %s, want 429 queue_full", code, body)
	}
	close(f.run.gate)
	// Drain the queue before the test ends: the worker keeps writing into
	// the temp dir that t.Cleanup removes otherwise.
	for _, id := range ids {
		testutil.WaitFor(t, func() bool {
			_, b := f.do(t, "GET", "/api/v1/builds/"+id, "")
			return strings.Contains(string(b), `"status":"success"`)
		}, "build "+id+" success")
	}
}

func TestCheckUpstreamError(t *testing.T) {
	// No worker: a triggered test build stays pending.
	f := newFixture(t, false)
	f.git.head = "1111111111111111111111111111111111111111"
	if code, _ := f.do(t, "POST", "/api/v1/check", ""); code != http.StatusAccepted {
		t.Fatalf("first check = %d", code)
	}
	// While a test build is pending, busy wins over any upstream error.
	f.git.headErr = errors.New("ls-remote failed")
	code, body := f.do(t, "POST", "/api/v1/check", "")
	if code != http.StatusConflict || !strings.Contains(string(body), "busy") {
		t.Errorf("busy check = %d %s, want 409 busy", code, body)
	}
	// With nothing in flight an upstream error is a 502.
	f2 := newFixture(t, false)
	f2.git.headErr = errors.New("ls-remote failed")
	code, body = f2.do(t, "POST", "/api/v1/check", "")
	if code != http.StatusBadGateway || !strings.Contains(string(body), "ls-remote failed") {
		t.Errorf("error check = %d %s, want 502", code, body)
	}
}

// TestErrorResponsesAreJSON: errors follow the {"error": ...} convention for
// 401 (auth), 404 (unknown path) and 405 (wrong method), and 405 carries
// Allow (issue #13).
func TestErrorResponsesAreJSON(t *testing.T) {
	f := newFixture(t, false)

	// 401 without a key.
	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("401 = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}

	// 404 unknown path.
	code, body := f.do(t, "GET", "/api/v1/nope", "")
	if code != http.StatusNotFound || !strings.Contains(string(body), `"error"`) {
		t.Errorf("unknown path = %d %s, want 404 json", code, body)
	}

	// 405 wrong method, with Allow.
	req = httptest.NewRequest("POST", "/healthz", nil)
	req.Header.Set("X-API-Key", "secret-key")
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" ||
		!strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("POST /healthz = %d %q %q, want 405 json Allow GET, HEAD", rec.Code, rec.Header().Get("Allow"), rec.Body.String())
	}

	// 405 on a POST route advertises just POST.
	code, body = f.do(t, "GET", "/api/v1/builds", "")
	if code != http.StatusMethodNotAllowed || !strings.Contains(string(body), `"error"`) {
		t.Errorf("GET /api/v1/builds = %d %s, want 405 json", code, body)
	}
	req = httptest.NewRequest("GET", "/api/v1/builds", nil)
	req.Header.Set("X-API-Key", "secret-key")
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Header().Get("Allow") != "POST" {
		t.Errorf("Allow on POST route = %q, want POST", rec.Header().Get("Allow"))
	}

	// HEAD is served wherever GET is.
	req = httptest.NewRequest("HEAD", "/healthz", nil)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD /healthz = %d, want 200", rec.Code)
	}

	// Unknown path without a key stays 401: route existence is not revealed.
	req = httptest.NewRequest("GET", "/api/v1/nope", nil)
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown path without key = %d, want 401", rec.Code)
	}
}

// TestBodyTooLarge: an oversized body is a 413, not a misleading 400
// (issue #13).
func TestBodyTooLarge(t *testing.T) {
	f := newFixture(t, false)
	big := `{"domain":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	code, body := f.do(t, "POST", "/api/v1/builds", big)
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), "too large") {
		t.Errorf("oversized body = %d %s, want 413", code, body)
	}
}
