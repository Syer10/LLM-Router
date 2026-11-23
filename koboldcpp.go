// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// KoboldCPP is a backend for one koboldcpp instance.
//
// When cfg.Command is set the router owns the process (spawn/restart/kill);
// otherwise it attaches to an already-running instance. Model switching uses
// the admin API (POST /api/admin/reload_config), so the process itself is
// never restarted for a model swap.
type KoboldCPP struct {
	cfg         BackendConfig
	swapTimeout time.Duration
	pm          *ProcessManager
	adminClient *http.Client // short timeout, control plane
	proxyClient *http.Client // no timeout, data plane

	mu          sync.Mutex // serializes load/unload operations
	stateMu     sync.RWMutex
	state       State
	loadedModel string
	protectedWarned bool
	models      []Model
	served      []string // names the backend reports it serves (discovery only)

	actMu   sync.Mutex
	lastAct time.Time
}

func NewKoboldCPP(cfg BackendConfig, swapTimeout time.Duration) *KoboldCPP {
	k := &KoboldCPP{
		cfg:         cfg,
		swapTimeout: swapTimeout,
		adminClient: &http.Client{Timeout: 30 * time.Second},
		proxyClient: &http.Client{},
		state:       StateStopped,
		lastAct:     time.Now(),
		models:      cfg.ExtraModelList(),
	}
	if cfg.Command != "" {
		k.pm = NewProcessManager(cfg.Name, cfg.Command, cfg.WorkDir, cfg.Env,
			cfg.BaseURL+cfg.HealthPath, cfg.StartTimeout)
	}
	return k
}

func (k *KoboldCPP) Name() string   { return k.cfg.Name }
func (k *KoboldCPP) Type() string   { return "koboldcpp" }
func (k *KoboldCPP) Touch()         { k.actMu.Lock(); k.lastAct = time.Now(); k.actMu.Unlock() }
func (k *KoboldCPP) LastActivity() time.Time {
	k.actMu.Lock()
	defer k.actMu.Unlock()
	return k.lastAct
}

func (k *KoboldCPP) setState(s State) {
	k.stateMu.Lock()
	k.state = s
	k.stateMu.Unlock()
}

func (k *KoboldCPP) State() State {
	k.stateMu.RLock()
	defer k.stateMu.RUnlock()
	return k.state
}

func (k *KoboldCPP) LoadedModel() string {
	k.stateMu.RLock()
	defer k.stateMu.RUnlock()
	return k.loadedModel
}

func (k *KoboldCPP) Models() []Model {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]Model, len(k.models))
	copy(out, k.models)
	return out
}

func (k *KoboldCPP) ServedModels() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]string, len(k.served))
	copy(out, k.served)
	return out
}

func (k *KoboldCPP) PID() int {
	if k.pm == nil {
		return 0
	}
	return k.pm.PID()
}

func (k *KoboldCPP) Logs(lines int) string {
	if k.pm == nil {
		return ""
	}
	return k.pm.Logs(lines)
}

func (k *KoboldCPP) StartProcess(ctx context.Context) error {
	if k.pm == nil {
		return ErrNoProcess
	}
	return k.pm.Start(ctx)
}

func (k *KoboldCPP) StopProcess() {
	if k.pm == nil {
		return
	}
	k.pm.Stop()
	k.setState(StateStopped)
}

func (k *KoboldCPP) adminReq(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.cfg.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if k.cfg.AdminKey != "" {
		req.Header.Set("Authorization", "Bearer "+k.cfg.AdminKey)
	}
	return k.adminClient.Do(req)
}

// protectedModelSentinel is what koboldcpp reports from /api/v1/model when
// --password is set and the Bearer token is missing or invalid.
const protectedModelSentinel = "koboldcpp/protected-model"

// currentModel reports whether a model is loaded and, best-effort, its name.
// A non-200, an empty model name, or a connection error means "not loaded /
// not reachable". The name comes from the "result" field (the documented
// shape of /api/v1/model); "id" is accepted as a fallback.
func (k *KoboldCPP) currentModel(ctx context.Context) (loaded bool, name string, err error) {
	resp, err := k.adminReq(ctx, http.MethodGet, "/api/v1/model", nil)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "", nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var info struct {
		Result string `json:"result"`
		ID     string `json:"id"`
	}
	if json.Unmarshal(data, &info) == nil {
		name = info.Result
		if name == "" {
			name = info.ID
		}
		if name == "" {
			return false, "", nil
		}
		return true, name, nil
	}
	// 200 with an unparseable body: the server is up and serving, assume loaded.
	return true, "", nil
}

// discoverNames lists available models via the admin API, falling back to
// the OpenAI /v1/models endpoint. Note: koboldcpp answers 200 with an empty
// body on /api/admin/list_options when the admin key is wrong, so an empty
// list also triggers the fallback.
func (k *KoboldCPP) discoverNames(ctx context.Context) ([]string, error) {
	var adminNames []string
	adminOK := false
	resp, err := k.adminReq(ctx, http.MethodGet, "/api/admin/list_options", nil)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&adminNames); err == nil {
				adminOK = true
			}
		}
	}
	if adminOK && len(adminNames) > 0 {
		return adminNames, nil
	}
	return k.openAIModelNames(ctx)
}

func (k *KoboldCPP) openAIModelNames(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.cfg.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if k.cfg.AdminKey != "" {
		req.Header.Set("Authorization", "Bearer "+k.cfg.AdminKey)
	}
	resp, err := k.adminClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		names = append(names, m.ID)
	}
	return names, nil
}

func (k *KoboldCPP) Discover(ctx context.Context) error {
	if k.pm != nil && !k.pm.Alive() {
		k.setState(StateStopped)
		return nil
	}
	names, err := k.discoverNames(ctx)
	if err != nil {
		k.setState(StateUnhealthy)
		return err
	}
	models := make([]Model, 0, len(names)+len(k.cfg.ExtraModels))
	served := make([]string, 0, len(names)+1)
	for _, n := range names {
		models = append(models, Model{Name: n, Backend: k.cfg.Name})
		served = append(served, n)
	}
	models = appendModelsUnique(models, k.cfg.ExtraModelList())

	if loaded, name, err := k.currentModel(ctx); err == nil {
		k.stateMu.Lock()
		k.loadedModel = name
		if loaded {
			k.state = StateReady
		} else {
			k.state = StateUnloaded
		}
		k.stateMu.Unlock()
		// The loaded model is reported by its GGUF name, which is not a
		// .kcppt in the list above. Add it so the running model can be
		// requested. Strip the "koboldcpp/" prefix that /api/v1/model adds:
		// a bare "/" would otherwise collide with the router's
		// "backend/model" syntax, and koboldcpp accepts the bare name.
		if loaded && name != "" && name != protectedModelSentinel {
			displayName := strings.TrimPrefix(name, "koboldcpp/")
			exists := false
			for _, m := range models {
				if m.Name == displayName {
					exists = true
					break
				}
			}
			if !exists {
				models = append(models, Model{Name: displayName, Backend: k.cfg.Name})
			}
			inServed := false
			for _, s := range served {
				if s == displayName {
					inServed = true
					break
				}
			}
			if !inServed {
				served = append(served, displayName)
			}
		}
	}

	k.mu.Lock()
	k.models = models
	k.served = served
	k.mu.Unlock()
	return nil
}

// EnsureLoaded makes the backend able to serve a request, starting the
// managed process first if needed. It does NOT load or swap models:
// koboldcpp loads the requested model itself when nothing is loaded, and does
// its own model routing otherwise. The router's only admin-API model action
// is the idle unload (FreeGPU). Concurrent callers serialize on k.mu.
func (k *KoboldCPP) EnsureLoaded(ctx context.Context, model string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.pm != nil && !k.pm.Alive() {
		k.setState(StateStarting)
		if err := k.pm.Start(ctx); err != nil {
			k.setState(StateUnhealthy)
			return fmt.Errorf("starting backend: %w", err)
		}
	}

	// Best-effort: refresh the tracked loaded-model name for the admin view.
	// This does not gate the forward — koboldcpp loads `model` itself if
	// nothing is loaded, so the request goes through either way.
	if loaded, name, err := k.currentModel(ctx); err == nil && loaded {
		if name == protectedModelSentinel && !k.protectedWarned {
			k.protectedWarned = true
			log.Printf("[%s] koboldcpp reports %q: set admin_key to the server password to track the real model name", k.cfg.Name, protectedModelSentinel)
		}
		k.stateMu.Lock()
		k.loadedModel = name
		k.state = StateReady
		k.stateMu.Unlock()
	} else {
		k.setState(StateReady)
	}
	return nil
}

// GPUActive reports whether a model is loaded (VRAM in use).
func (k *KoboldCPP) GPUActive() bool {
	s := k.State()
	return s == StateReady || s == StateSwapping
}

// FreeGPU unloads the current model (VRAM is freed, the process stays up).
func (k *KoboldCPP) FreeGPU(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.LoadedModel() == "" {
		return nil
	}
	// In-transition: the idle loop only fires from StateReady, so this
	// prevents re-triggering (and overlapping releases) while we work.
	k.setState(StateSwapping)

	if resp, err := k.adminReq(ctx, http.MethodPost, "/api/admin/reload_config",
		map[string]string{"filename": "unload_model"}); err == nil {
		resp.Body.Close()
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if loaded, _, err := k.currentModel(ctx); err == nil && !loaded {
			k.stateMu.Lock()
			k.loadedModel = ""
			k.state = StateUnloaded
			k.stateMu.Unlock()
			return nil
		}
	}
	// Timed out: the model is presumably still loaded.
	k.setState(StateReady)
	return fmt.Errorf("timed out waiting for model to unload")
}

func (k *KoboldCPP) Forward(ctx context.Context, method, subpath string, body []byte, headers http.Header) (*http.Response, error) {
	if k.pm != nil && !k.pm.Alive() {
		return nil, fmt.Errorf("backend process is not running")
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.cfg.BaseURL+"/v1"+subpath, rdr)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req, headers)
	if k.cfg.AdminKey != "" {
		req.Header.Set("Authorization", "Bearer "+k.cfg.AdminKey)
	}
	return k.proxyClient.Do(req)
}
