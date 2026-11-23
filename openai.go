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
	"net/http"
	"sync"
	"time"
)

// OpenAIBackend is a generic OpenAI-compatible backend: remote API
// (OpenAI, OpenRouter, ...) or a local server (vLLM, llama.cpp server,
// LM Studio, ...). When cfg.Command is set the router owns the process.
//
// base_url is the OpenAI root, e.g. http://127.0.0.1:8000/v1.
type OpenAIBackend struct {
	cfg BackendConfig
	pm  *ProcessManager

	stateMu sync.RWMutex
	state   State
	models  []Model

	actMu   sync.Mutex
	lastAct time.Time
}

func NewOpenAIBackend(cfg BackendConfig) *OpenAIBackend {
	o := &OpenAIBackend{cfg: cfg, state: StateStopped, lastAct: time.Now()}
	if cfg.Command != "" {
		o.pm = NewProcessManager(cfg.Name, cfg.Command, cfg.WorkDir, cfg.Env,
			cfg.BaseURL+cfg.HealthPath, cfg.StartTimeout)
	}
	return o
}

func (o *OpenAIBackend) Name() string   { return o.cfg.Name }
func (o *OpenAIBackend) Type() string   { return "openai" }
func (o *OpenAIBackend) Touch()         { o.actMu.Lock(); o.lastAct = time.Now(); o.actMu.Unlock() }
func (o *OpenAIBackend) LastActivity() time.Time {
	o.actMu.Lock()
	defer o.actMu.Unlock()
	return o.lastAct
}

func (o *OpenAIBackend) setState(s State) {
	o.stateMu.Lock()
	o.state = s
	o.stateMu.Unlock()
}

func (o *OpenAIBackend) State() State {
	o.stateMu.RLock()
	defer o.stateMu.RUnlock()
	return o.state
}

func (o *OpenAIBackend) LoadedModel() string { return "" }

func (o *OpenAIBackend) Models() []Model {
	o.stateMu.RLock()
	defer o.stateMu.RUnlock()
	out := make([]Model, len(o.models))
	copy(out, o.models)
	return out
}

func (o *OpenAIBackend) PID() int {
	if o.pm == nil {
		return 0
	}
	return o.pm.PID()
}

func (o *OpenAIBackend) Logs(lines int) string {
	if o.pm == nil {
		return ""
	}
	return o.pm.Logs(lines)
}

func (o *OpenAIBackend) StartProcess(ctx context.Context) error {
	if o.pm == nil {
		return ErrNoProcess
	}
	return o.pm.Start(ctx)
}

func (o *OpenAIBackend) StopProcess() {
	if o.pm == nil {
		return
	}
	o.pm.Stop()
	o.setState(StateStopped)
}

func (o *OpenAIBackend) Discover(ctx context.Context) error {
	if o.pm != nil && !o.pm.Alive() {
		o.setState(StateStopped)
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.cfg.BaseURL+"/models", nil)
	if err != nil {
		return err
	}
	if o.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		o.setState(StateUnhealthy)
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		o.setState(StateUnhealthy)
		return fmt.Errorf("decoding models: %w", err)
	}
	models := make([]Model, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, Model{Name: m.ID, Backend: o.cfg.Name})
	}
	o.stateMu.Lock()
	o.models = models
	o.state = StateReady
	o.stateMu.Unlock()
	return nil
}

// EnsureLoaded starts the managed process if needed. OpenAI-compatible
// servers serve whatever models they have; there is nothing to swap.
func (o *OpenAIBackend) EnsureLoaded(ctx context.Context, model string) error {
	if o.pm != nil && !o.pm.Alive() {
		o.setState(StateStarting)
		if err := o.pm.Start(ctx); err != nil {
			o.setState(StateUnhealthy)
			return fmt.Errorf("starting backend: %w", err)
		}
	}
	o.setState(StateReady)
	return nil
}

// GPUActive reports whether the owned process is running (its model is in
// VRAM). Attached backends are not tracked, so they report false — the router
// cannot see or free them.
func (o *OpenAIBackend) GPUActive() bool {
	if o.pm == nil {
		return false
	}
	return o.PID() != 0
}

// FreeGPU stops the owned process, freeing VRAM. Attached backends are left
// alone (the router cannot free them).
func (o *OpenAIBackend) FreeGPU(ctx context.Context) error {
	if o.pm == nil {
		return nil
	}
	o.StopProcess()
	return nil
}

// Release stops the managed process (remote backends are left alone).
func (o *OpenAIBackend) Release(ctx context.Context) error {
	if o.pm != nil {
		o.pm.Stop()
		o.setState(StateStopped)
	}
	return nil
}

func (o *OpenAIBackend) Forward(ctx context.Context, method, subpath string, body []byte, headers http.Header) (*http.Response, error) {
	if o.pm != nil && !o.pm.Alive() {
		return nil, fmt.Errorf("backend process is not running")
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.cfg.BaseURL+subpath, rdr)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req, headers)
	if o.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}
	return (&http.Client{}).Do(req)
}
