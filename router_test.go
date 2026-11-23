// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
)

// --- mock koboldcpp ---

type mockKCPP struct {
	mu          sync.Mutex
	loaded      string
	options     []string
	reloadDelay time.Duration
	reloads     int
	// When set, /api/v1/model answers 200 with an empty result while
	// nothing is loaded (like real koboldcpp does).
	emptyResultWhenUnloaded bool
	// When set, /api/v1/model always answers the protected-model sentinel
	// (what koboldcpp returns when --password is set and the key is bad).
	protected bool
}

func (m *mockKCPP) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/list_options", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		opts := append([]string{}, m.options...)
		m.mu.Unlock()
		json.NewEncoder(w).Encode(opts)
	})
	mux.HandleFunc("/api/v1/model", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		loaded := m.loaded
		empty := m.emptyResultWhenUnloaded
		protected := m.protected
		m.mu.Unlock()
		if protected {
			json.NewEncoder(w).Encode(map[string]any{"result": "koboldcpp/protected-model"})
			return
		}
		if loaded == "" {
			if empty {
				json.NewEncoder(w).Encode(map[string]any{"result": ""})
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"no model loaded"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"result": loaded})
	})
	mux.HandleFunc("/api/admin/reload_config", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Filename       string `json:"filename"`
			OverrideConfig string `json:"overrideconfig"`
			BaseConfig     string `json:"baseconfig"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		m.mu.Lock()
		m.reloads++
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"success": true})
		go func() {
			time.Sleep(m.reloadDelay)
			m.mu.Lock()
			if in.Filename == "unload_model" {
				m.loaded = ""
			} else {
				m.loaded = in.Filename
			}
			m.mu.Unlock()
		}()
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		loaded := m.loaded
		// Koboldcpp loads the requested model itself when nothing is loaded.
		if loaded == "" && req.Model != "" {
			m.loaded = req.Model
			loaded = req.Model
		}
		m.mu.Unlock()
		if loaded == "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"no model loaded"}`)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello from %s\"}}]}\n\n", loaded)
			fl.Flush()
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "hello from " + loaded}}},
		})
	})
	// A non-completion endpoint (Responses API) to prove the router routes
	// arbitrary OpenAI paths, not just chat/completions.
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "ok": true})
	})
	return mux
}

// --- helpers ---

func doRequest(t *testing.T, r *Router, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	if strings.HasPrefix(path, "/admin") {
		r.HandleAdmin(w, req)
	} else {
		r.HandleV1(w, req)
	}
	return w
}

func chatBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, model, stream)
}

func TestKoboldCPPLoadNoSwapUnload(t *testing.T) {
	mock := &mockKCPP{options: []string{"A.kcppt", "B.kcppt"}, reloadDelay: 200 * time.Millisecond}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := &Config{
		Backends: []BackendConfig{{Name: "kobold", Type: "koboldcpp", BaseURL: srv.URL}},
		Aliases:  map[string]string{"a": "kobold/A.kcppt"},
	}
	cfg.SwapTimeout = 30 * time.Second
	r := NewRouter(cfg)
	if err := r.backends["kobold"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// Model list is discovered from the backend, not the config.
	rec := doRequest(t, r, "GET", "/v1/models", "")
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	if !ids["A.kcppt"] || !ids["B.kcppt"] {
		t.Fatalf("expected A.kcppt and B.kcppt in model list, got %v", ids)
	}

	// First request: koboldcpp loads the model itself (the router does not
	// issue a load). Refresh the tracked state, then check the loaded name.
	rec = doRequest(t, r, "POST", "/v1/chat/completions", chatBody("a", false))
	if rec.Code != 200 {
		t.Fatalf("chat A: status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello from A.kcppt") {
		t.Fatalf("chat A: unexpected body %s", rec.Body.String())
	}
	if err := r.backends["kobold"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if got := r.backends["kobold"].LoadedModel(); got != "A.kcppt" {
		t.Fatalf("loaded model = %q, want A.kcppt", got)
	}

	// Second request for B does NOT swap: a model (A) is already loaded, so
	// the router forwards and lets koboldcpp do its own model routing. The
	// response is served by the loaded model (A), not B.
	rec = doRequest(t, r, "POST", "/v1/chat/completions", chatBody("B.kcppt", false))
	if rec.Code != 200 {
		t.Fatalf("chat B: status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello from A.kcppt") {
		t.Fatalf("chat B: expected no-swap (served by loaded A.kcppt), got %s", rec.Body.String())
	}

	// Streaming passes through.
	rec = doRequest(t, r, "POST", "/v1/chat/completions", chatBody("B.kcppt", true))
	if rec.Code != 200 {
		t.Fatalf("stream: status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream body missing [DONE]: %s", rec.Body.String())
	}

	// Unknown model -> 404.
	rec = doRequest(t, r, "POST", "/v1/chat/completions", chatBody("nope.kcppt", false))
	if rec.Code != 404 {
		t.Fatalf("unknown model: status %d, want 404", rec.Code)
	}

	// Admin: unload.
	rec = doRequest(t, r, "POST", "/admin/backends/kobold/unload", "")
	if rec.Code != 200 {
		t.Fatalf("unload: status %d: %s", rec.Code, rec.Body.String())
	}
	mock.mu.Lock()
	loaded := mock.loaded
	mock.mu.Unlock()
	if loaded != "" {
		t.Fatalf("mock still has model loaded: %q", loaded)
	}

	// Admin: list shows state.
	rec = doRequest(t, r, "GET", "/admin/backends", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"kobold"`) {
		t.Fatalf("admin list: %d %s", rec.Code, rec.Body.String())
	}
}

func TestKoboldCPPLoadedGGUFNameInList(t *testing.T) {
	// The loaded model is reported by its GGUF name (with a "koboldcpp/"
	// prefix), which is not a .kcppt in list_options. The router must add
	// it to the model list (prefix stripped) so the running model can be
	// requested without a 404, and must not swap when it is requested.
	mock := &mockKCPP{
		options: []string{"A.kcppt"},
		loaded:  "koboldcpp/MyModel-Q4_K_M",
	}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := &Config{Backends: []BackendConfig{{Name: "kobold", Type: "koboldcpp", BaseURL: srv.URL}}}
	cfg.SwapTimeout = 30 * time.Second
	r := NewRouter(cfg)
	if err := r.backends["kobold"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	rec := doRequest(t, r, "GET", "/v1/models", "")
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	if !ids["MyModel-Q4_K_M"] {
		t.Fatalf("expected loaded GGUF name MyModel-Q4_K_M in model list, got %v", ids)
	}
	if ids["koboldcpp/MyModel-Q4_K_M"] {
		t.Fatalf("model list should not contain the prefixed name")
	}

	// Requesting the GGUF name works (no 404) and does not swap.
	rec = doRequest(t, r, "POST", "/v1/chat/completions", chatBody("MyModel-Q4_K_M", false))
	if rec.Code != 200 {
		t.Fatalf("chat GGUF name: status %d: %s", rec.Code, rec.Body.String())
	}
	mock.mu.Lock()
	reloads := mock.reloads
	mock.mu.Unlock()
	if reloads != 0 {
		t.Fatalf("requesting the loaded model issued %d reloads, want 0 (no swap)", reloads)
	}
}

func TestKoboldCPPConcurrentRequestsQueueBehindOneSwap(t *testing.T) {
	mock := &mockKCPP{options: []string{"A.kcppt"}, reloadDelay: 500 * time.Millisecond}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := &Config{Backends: []BackendConfig{{Name: "kobold", Type: "koboldcpp", BaseURL: srv.URL}}}
	cfg.SwapTimeout = 30 * time.Second
	r := NewRouter(cfg)
	if err := r.backends["kobold"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := doRequest(t, r, "POST", "/v1/chat/completions", chatBody("A.kcppt", false))
			if rec.Code != 200 {
				errs <- fmt.Errorf("status %d: %s", rec.Code, rec.Body.String())
				return
			}
			if !strings.Contains(rec.Body.String(), "hello from A.kcppt") {
				errs <- fmt.Errorf("unexpected body %s", rec.Body.String())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// --- mock openai backend ---

func TestOpenAIForwardAndAlias(t *testing.T) {
	var gotModel string
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "gpt-test"}}})
		case "/v1/chat/completions":
			var req struct {
				Model string `json:"model"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			gotModel = req.Model
			json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer oa.Close()

	cfg := &Config{
		Backends: []BackendConfig{{Name: "cloud", Type: "openai", BaseURL: oa.URL + "/v1"}},
		Aliases:  map[string]string{"gpt": "cloud/gpt-test"},
	}
	r := NewRouter(cfg)
	if err := r.backends["cloud"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	rec := doRequest(t, r, "POST", "/v1/chat/completions", chatBody("gpt", false))
	if rec.Code != 200 {
		t.Fatalf("chat: status %d: %s", rec.Code, rec.Body.String())
	}
	if gotModel != "gpt-test" {
		t.Fatalf("backend saw model %q, want gpt-test", gotModel)
	}
}

func TestKoboldCPPGenericEndpoint(t *testing.T) {
	// A non-completion endpoint (the Responses API) is routed to the backend
	// and the model is rewritten, proving the router handles arbitrary
	// OpenAI paths, not just chat/completions.
	mock := &mockKCPP{options: []string{"A.kcppt"}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := &Config{Backends: []BackendConfig{{Name: "kobold", Type: "koboldcpp", BaseURL: srv.URL}}}
	r := NewRouter(cfg)
	if err := r.backends["kobold"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	rec := doRequest(t, r, "POST", "/v1/responses", `{"model":"A.kcppt","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("responses: status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"model":"A.kcppt"`) {
		t.Fatalf("responses body: %s", rec.Body.String())
	}
}

func TestGenericPassthrough(t *testing.T) {
	// The router is a transparent proxy: it routes any OpenAI path, preserves
	// method/path/query, rewrites the model when present, aggregates the model
	// list, serves single-model lookups, and falls back to the first backend
	// for model-less requests.
	type got struct {
		method string
		path   string
		model  string
	}
	var mu sync.Mutex
	var last got
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var model string
		if r.Body != nil {
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			model, _ = p["model"].(string)
		}
		mu.Lock()
		last = got{method: r.Method, path: r.URL.Path, model: model}
		mu.Unlock()
		switch {
		case r.URL.Path == "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "m1"}}})
		case strings.HasPrefix(r.URL.Path, "/v1/models/"):
			json.NewEncoder(w).Encode(map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/v1/models/"), "object": "model"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "model": model})
		}
	}))
	defer srv.Close()

	cfg := &Config{Backends: []BackendConfig{{Name: "cloud", Type: "openai", BaseURL: srv.URL + "/v1"}}}
	r := NewRouter(cfg)
	if err := r.backends["cloud"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// POST /v1/responses with a model: routed, model rewritten, method+path kept.
	rec := doRequest(t, r, "POST", "/v1/responses", `{"model":"m1","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("responses: status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if last.method != "POST" || last.path != "/v1/responses" || last.model != "m1" {
		t.Fatalf("responses: backend saw %s %s model=%q", last.method, last.path, last.model)
	}
	mu.Unlock()

	// GET /v1/models/{id}: single model retrieve.
	rec = doRequest(t, r, "GET", "/v1/models/m1", "")
	if rec.Code != 200 {
		t.Fatalf("model retrieve: status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"m1"`) {
		t.Fatalf("model retrieve body: %s", rec.Body.String())
	}

	// GET /v1/files (no model): routed to the default backend.
	rec = doRequest(t, r, "GET", "/v1/files", "")
	if rec.Code != 200 {
		t.Fatalf("files: status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if last.method != "GET" || last.path != "/v1/files" {
		t.Fatalf("files: backend saw %s %s", last.method, last.path)
	}
	mu.Unlock()

	// Root-level path without the /v1 prefix is normalized to /v1 for the backend.
	rec = doRequest(t, r, "POST", "/responses", `{"model":"m1","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("root responses: status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if last.path != "/v1/responses" {
		t.Fatalf("root responses: backend saw path %s, want /v1/responses", last.path)
	}
	mu.Unlock()

	// Unknown model in a model-bearing request -> 404.
	rec = doRequest(t, r, "POST", "/v1/responses", `{"model":"nope","input":"hi"}`)
	if rec.Code != 404 {
		t.Fatalf("unknown model: status %d, want 404", rec.Code)
	}
}

// flakyBackend fails its first N Forward calls with failErr, then succeeds.
// Only Name and Forward are used; the embedded interface is never called.
type flakyBackend struct {
	Backend
	fails   int
	calls   int
	failErr error
}

func (f *flakyBackend) Name() string { return "flaky" }

func (f *flakyBackend) Forward(ctx context.Context, method, subpath string, body []byte, headers http.Header) (*http.Response, error) {
	f.calls++
	if f.calls <= f.fails {
		return nil, f.failErr
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteString(`{"ok":true}`)
	return rec.Result(), nil
}

func TestForwardWithRetry(t *testing.T) {
	stale := &url.Error{Op: "Post", URL: "http://flaky/v1/x", Err: errors.New("http: server closed idle connection")}

	// Transient (stale keep-alive) error: retried once, then succeeds.
	f := &flakyBackend{fails: 1, failErr: stale}
	resp, err := forwardWithRetry(context.Background(), f, "POST", "/chat/completions", []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("transient: %v", err)
	}
	if resp.StatusCode != 200 || f.calls != 2 {
		t.Fatalf("transient: status=%d calls=%d, want 200/2", resp.StatusCode, f.calls)
	}

	// Non-transient error: NOT retried.
	g := &flakyBackend{fails: 1, failErr: errors.New("boom")}
	if _, err := forwardWithRetry(context.Background(), g, "POST", "/x", nil, nil); err == nil {
		t.Fatalf("non-transient: expected an error")
	}
	if g.calls != 1 {
		t.Fatalf("non-transient: Forward called %d times, want 1 (no retry)", g.calls)
	}

	// Transient error that also fails on the retry: give up after one retry.
	h := &flakyBackend{fails: 5, failErr: &url.Error{Op: "Post", URL: "http://flaky/v1/x", Err: errors.New("broken pipe")}}
	if _, err := forwardWithRetry(context.Background(), h, "POST", "/x", nil, nil); err == nil {
		t.Fatalf("persistent: expected an error")
	}
	if h.calls != 2 {
		t.Fatalf("persistent: Forward called %d times, want 2 (one retry then give up)", h.calls)
	}
}

func TestBodySizeLimit(t *testing.T) {
	// The request-body cap is a config option (max_body_bytes). A body over
	// the cap is rejected with 413 instead of being read into memory
	// (memory-exhaustion DoS guard); a body under it is accepted.
	mock := &mockKCPP{options: []string{"A.kcppt"}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := &Config{
		Backends:     []BackendConfig{{Name: "kobold", Type: "koboldcpp", BaseURL: srv.URL}},
		MaxBodyBytes: 1024, // small cap so the test stays fast
	}
	r := NewRouter(cfg)
	if err := r.backends["kobold"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// Over the cap -> 413.
	big := strings.Repeat("a", 1025)
	rec := doRequest(t, r, "POST", "/v1/chat/completions", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status %d, want 413", rec.Code)
	}

	// Under the cap -> accepted.
	rec = doRequest(t, r, "POST", "/v1/chat/completions", chatBody("A.kcppt", false))
	if rec.Code != 200 {
		t.Fatalf("small body: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestKoboldCPPUnloadWithEmptyModelID(t *testing.T) {
	// Regression: koboldcpp answers 200 with an empty result on
	// /api/v1/model when nothing is loaded; Release must still confirm
	// the unload.
	mock := &mockKCPP{options: []string{"A.kcppt"}, reloadDelay: 100 * time.Millisecond, emptyResultWhenUnloaded: true}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := &Config{Backends: []BackendConfig{{Name: "kobold", Type: "koboldcpp", BaseURL: srv.URL}}}
	cfg.SwapTimeout = 30 * time.Second
	r := NewRouter(cfg)
	if err := r.backends["kobold"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	k := r.backends["kobold"].(*KoboldCPP)

	// Load the model by sending a request (koboldcpp loads it itself; the
	// router does not issue a load). Then refresh the tracked state.
	rec := doRequest(t, r, "POST", "/v1/chat/completions", chatBody("A.kcppt", false))
	if rec.Code != 200 {
		t.Fatalf("chat: status %d: %s", rec.Code, rec.Body.String())
	}
	if err := k.Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if k.State() != StateReady {
		t.Fatalf("state = %v, want ready", k.State())
	}
	if k.LoadedModel() != "A.kcppt" {
		t.Fatalf("loaded model = %q, want A.kcppt", k.LoadedModel())
	}

	if err := k.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if k.State() != StateUnloaded {
		t.Fatalf("state = %v, want unloaded", k.State())
	}
	if k.LoadedModel() != "" {
		t.Fatalf("loaded model = %q, want empty", k.LoadedModel())
	}
}

func TestKoboldCPPProtectedModelSentinel(t *testing.T) {
	// With --password set and a bad/missing admin key, /api/v1/model
	// answers the protected sentinel. The router must not treat that as
	// "nothing loaded" (which would reload on every request) and must not
	// attempt swaps it cannot verify.
	mock := &mockKCPP{options: []string{"A.kcppt"}, reloadDelay: 100 * time.Millisecond, protected: true}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := &Config{Backends: []BackendConfig{{Name: "kobold", Type: "koboldcpp", BaseURL: srv.URL}}}
	cfg.SwapTimeout = 30 * time.Second
	r := NewRouter(cfg)
	k := r.backends["kobold"].(*KoboldCPP)
	if err := k.Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if k.State() != StateReady {
		t.Fatalf("state = %v, want ready (sentinel means a model is loaded)", k.State())
	}

	if err := k.EnsureLoaded(context.Background(), "A.kcppt"); err != nil {
		t.Fatalf("ensure loaded: %v", err)
	}
	mock.mu.Lock()
	reloads := mock.reloads
	mock.mu.Unlock()
	if reloads != 0 {
		t.Fatalf("ensure loaded issued %d reloads, want 0 (swap unverifiable while protected)", reloads)
	}
}

func TestAuth(t *testing.T) {
	mock := &mockKCPP{options: []string{"A.kcppt"}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	cfg := &Config{
		Backends: []BackendConfig{{Name: "kobold", Type: "koboldcpp", BaseURL: srv.URL}},
		APIKeys:  []string{"secret-key"},
	}
	cfg.SwapTimeout = 30 * time.Second
	r := NewRouter(cfg)
	if err := r.backends["kobold"].Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// No key -> 401 on /v1.
	rec := doRequest(t, r, "GET", "/v1/models", "")
	if rec.Code != 401 {
		t.Fatalf("no key: status %d, want 401", rec.Code)
	}

	// Wrong key -> 401.
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	r.HandleV1(w, req)
	if w.Code != 401 {
		t.Fatalf("wrong key: status %d, want 401", w.Code)
	}

	// Correct key via Bearer -> 200.
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	w = httptest.NewRecorder()
	r.HandleV1(w, req)
	if w.Code != 200 {
		t.Fatalf("bearer key: status %d, want 200: %s", w.Code, w.Body.String())
	}

	// Correct key via x-api-key -> 200.
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("X-Api-Key", "secret-key")
	w = httptest.NewRecorder()
	r.HandleV1(w, req)
	if w.Code != 200 {
		t.Fatalf("x-api-key: status %d, want 200", w.Code)
	}

	// Admin API is protected too.
	rec = doRequest(t, r, "GET", "/admin/backends", "")
	if rec.Code != 401 {
		t.Fatalf("admin no key: status %d, want 401", rec.Code)
	}
	rec = doRequest(t, r, "POST", "/admin/backends/kobold/unload", "")
	if rec.Code != 401 {
		t.Fatalf("admin post no key: status %d, want 401", rec.Code)
	}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`koboldcpp -m model.gguf --openai -port 5001`: {
			"koboldcpp", "-m", "model.gguf", "--openai", "-port", "5001",
		},
		`vllm serve "/models/my model.gguf" --port 8000`: {
			"vllm", "serve", "/models/my model.gguf", "--port", "8000",
		},
		`  spaced   out  `: {"spaced", "out"},
		"": {},
	}
	for in, want := range cases {
		got := splitCommand(in)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("splitCommand(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolveBareNameAcrossBackends(t *testing.T) {
	cfg := &Config{
		Backends: []BackendConfig{
			{Name: "a", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{Name: "b", Type: "openai", BaseURL: "http://127.0.0.1:2/v1"},
		},
	}
	r := NewRouter(cfg)
	for name, owner := range map[string]string{"a": "a", "b": "b"} {
		oa := r.backends[name].(*OpenAIBackend)
		oa.stateMu.Lock()
		oa.models = []Model{{Name: "shared", Backend: owner}}
		oa.stateMu.Unlock()
	}

	b, model, err := r.Resolve("shared")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b.Name() != "a" || model != "shared" {
		t.Fatalf("resolved %s/%s, want a/shared (first backend wins)", b.Name(), model)
	}

	b, model, err = r.Resolve("b/shared")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b.Name() != "b" || model != "shared" {
		t.Fatalf("resolved %s/%s, want b/shared", b.Name(), model)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestGPUIdle(t *testing.T) {
	mock := &mockKCPP{options: []string{"A.kcppt"}}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	// A remote openai backend (no local GPU) and an attached openai backend
	// (GPU-marked but the router cannot see it) must not affect GPUIdle.
	cfg := &Config{
		Backends: []BackendConfig{
			{Name: "kobold", Type: "koboldcpp", BaseURL: srv.URL},
			{Name: "cloud", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"},
			{Name: "attached", Type: "openai", BaseURL: "http://127.0.0.1:2/v1", UsesLocalGPU: boolPtr(true)},
		},
	}
	r := NewRouter(cfg)
	k := r.backends["kobold"].(*KoboldCPP)
	if err := k.Discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}

	// No model loaded -> the GPU is free (cloud and attached are ignored).
	if !r.GPUIdle() {
		t.Fatalf("GPUIdle = false, want true (no model loaded)")
	}

	// A loaded model -> the GPU is busy.
	k.stateMu.Lock()
	k.state = StateReady
	k.loadedModel = "A.kcppt"
	k.stateMu.Unlock()
	if r.GPUIdle() {
		t.Fatalf("GPUIdle = true, want false (model loaded)")
	}
}

func TestFreeOtherGPU(t *testing.T) {
	// Two koboldcpp backends; A has a model loaded (active), B does not.
	// freeOtherGPU(keep=B) releases A but leaves B alone.
	mockA := &mockKCPP{options: []string{"A.kcppt"}, loaded: "A.kcppt", reloadDelay: 100 * time.Millisecond}
	srvA := httptest.NewServer(mockA.handler())
	defer srvA.Close()
	mockB := &mockKCPP{options: []string{"B.kcppt"}, reloadDelay: 100 * time.Millisecond}
	srvB := httptest.NewServer(mockB.handler())
	defer srvB.Close()

	cfg := &Config{
		Backends: []BackendConfig{
			{Name: "a", Type: "koboldcpp", BaseURL: srvA.URL},
			{Name: "b", Type: "koboldcpp", BaseURL: srvB.URL},
		},
	}
	r := NewRouter(cfg)
	r.backends["a"].Discover(context.Background())
	r.backends["b"].Discover(context.Background())

	if !r.backends["a"].GPUActive() {
		t.Fatalf("a should be active (model loaded)")
	}
	if r.backends["b"].GPUActive() {
		t.Fatalf("b should not be active")
	}

	r.freeOtherGPU(r.backends["b"])

	if r.backends["a"].GPUActive() {
		t.Fatalf("a should be released after freeOtherGPU")
	}
	if r.backends["b"].GPUActive() {
		t.Fatalf("b should be untouched")
	}
}

func TestOpenAIGPUAttachedNoOp(t *testing.T) {
	// An attached openai backend (no owned process) is excluded from GPU
	// arbitration: it reports inactive and FreeGPU is a no-op.
	o := NewOpenAIBackend(BackendConfig{Name: "x", Type: "openai", BaseURL: "http://127.0.0.1:1/v1"})
	if o.GPUActive() {
		t.Fatalf("attached openai GPUActive = true, want false")
	}
	if err := o.FreeGPU(context.Background()); err != nil {
		t.Fatalf("attached openai FreeGPU: %v", err)
	}
}

func TestJobTickDecision(t *testing.T) {
	spec, err := cron.ParseStandard("* * * * *")
	if err != nil {
		t.Fatalf("parse schedule: %v", err)
	}
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	newState := func(job Job) *jobState {
		return &jobState{job: job, spec: spec, next: base, logs: newLogBuffer(1024)}
	}

	// Due + idle -> run.
	if got := newState(Job{OnBusy: "defer"}).tickOnce(base, true); got != actRun {
		t.Fatalf("due+idle: got %v, want run", got)
	}
	// Due + busy + defer (no max_wait) -> keep waiting.
	if got := newState(Job{OnBusy: "defer"}).tickOnce(base, false); got != actDefer {
		t.Fatalf("due+busy+defer: got %v, want defer", got)
	}
	// Due + busy + skip -> skip.
	if got := newState(Job{OnBusy: "skip"}).tickOnce(base, false); got != actSkip {
		t.Fatalf("due+busy+skip: got %v, want skip", got)
	}
	// Due + busy + defer past max_wait -> skip.
	js := newState(Job{OnBusy: "defer", MaxWait: time.Minute})
	js.mu.Lock()
	js.scheduledAt = base
	js.mu.Unlock()
	if got := js.tickOnce(base.Add(2*time.Minute), false); got != actSkip {
		t.Fatalf("due+busy+defer+maxwait: got %v, want skip", got)
	}
	// A not-yet-due job is left alone.
	js = &jobState{job: Job{OnBusy: "defer"}, spec: spec, next: base.Add(time.Hour), logs: newLogBuffer(1024)}
	if got := js.tickOnce(base, true); got != actNone {
		t.Fatalf("not due: got %v, want none", got)
	}
}

func TestJobAdminEndpoints(t *testing.T) {
	cfg := &Config{
		Backends: []BackendConfig{{Name: "kobold", Type: "koboldcpp", BaseURL: "http://127.0.0.1:1"}},
		Jobs: []Job{{
			Name: "train", Schedule: "0 2 * * *", Command: "echo hi", OnBusy: "defer",
		}},
	}
	r := NewRouter(cfg)

	// List.
	rec := doRequest(t, r, "GET", "/admin/jobs", "")
	if rec.Code != 200 {
		t.Fatalf("list: status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"train"`) {
		t.Fatalf("list body: %s", rec.Body.String())
	}

	// Single job.
	rec = doRequest(t, r, "GET", "/admin/jobs/train", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"schedule":"0 2 * * *"`) {
		t.Fatalf("one: status %d: %s", rec.Code, rec.Body.String())
	}

	// Logs (empty).
	rec = doRequest(t, r, "GET", "/admin/jobs/train/logs", "")
	if rec.Code != 200 {
		t.Fatalf("logs: status %d: %s", rec.Code, rec.Body.String())
	}

	// Unknown job -> 404.
	rec = doRequest(t, r, "GET", "/admin/jobs/nope", "")
	if rec.Code != 404 {
		t.Fatalf("unknown job: status %d, want 404", rec.Code)
	}
}
