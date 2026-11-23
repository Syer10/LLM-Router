// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type State int

const (
	StateStopped State = iota
	StateStarting
	StateReady
	StateSwapping
	StateUnloaded
	StateUnhealthy
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateSwapping:
		return "swapping"
	case StateUnloaded:
		return "unloaded"
	case StateUnhealthy:
		return "unhealthy"
	}
	return "unknown"
}

type Model struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
}

var ErrNoProcess = fmt.Errorf("backend has no managed process")

// Backend is one upstream engine (koboldcpp instance, OpenAI-compatible
// server, remote API).
type Backend interface {
	Name() string
	Type() string
	State() State
	LoadedModel() string
	Models() []Model
	// PID of the managed process, 0 when the router does not own one.
	PID() int
	Logs(lines int) string

	// Discover refreshes the model list and current state.
	Discover(ctx context.Context) error
	// EnsureLoaded makes the backend able to serve a request: a no-op when a
	// model is already loaded (the backend does its own model routing), a
	// load of `model` when nothing is loaded, or a process start. It does
	// not swap models. Serializes internally.
	EnsureLoaded(ctx context.Context, model string) error
	// Release frees resources: unloads the model (koboldcpp) or stops the
	// process (owned backends).
	Release(ctx context.Context) error
	// Forward sends an OpenAI request to the backend. method is the HTTP
	// method (POST, GET, ...); subpath is relative to the backend's OpenAI
	// root and may carry a query string, e.g. "/chat/completions?stream=true".
	// body is nil for requests without a body.
	Forward(ctx context.Context, method, subpath string, body []byte, headers http.Header) (*http.Response, error)

	StartProcess(ctx context.Context) error
	StopProcess()

	Touch()
	LastActivity() time.Time
}

// Router is the model registry and resolver.
type Router struct {
	cfg      *Config
	backends map[string]Backend
	cfgs     map[string]BackendConfig
	order    []string
}

func NewRouter(cfg *Config) *Router {
	r := &Router{
		cfg:      cfg,
		backends: map[string]Backend{},
		cfgs:     map[string]BackendConfig{},
	}
	for _, bc := range cfg.Backends {
		var b Backend
		switch bc.Type {
		case "koboldcpp":
			b = NewKoboldCPP(bc, cfg.SwapTimeout)
		case "openai":
			b = NewOpenAIBackend(bc)
		}
		r.backends[bc.Name] = b
		r.cfgs[bc.Name] = bc
		r.order = append(r.order, bc.Name)
	}
	return r
}

// Resolve maps a client model name to (backend, real model name).
// Accepts: alias, "backend/model", or a bare name found on any backend.
func (r *Router) Resolve(model string) (Backend, string, error) {
	if model == "" {
		return nil, "", fmt.Errorf("model is required")
	}
	if target, ok := r.cfg.Aliases[model]; ok {
		return r.resolveTarget(target)
	}
	if strings.Contains(model, "/") {
		return r.resolveTarget(model)
	}
	for _, name := range r.order {
		for _, m := range r.backends[name].Models() {
			if m.Name == model {
				return r.backends[name], model, nil
			}
		}
	}
	return nil, "", fmt.Errorf("model %q not found on any backend", model)
}

func (r *Router) resolveTarget(target string) (Backend, string, error) {
	parts := strings.SplitN(target, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, "", fmt.Errorf("invalid model target %q (want backend/model)", target)
	}
	b, ok := r.backends[parts[0]]
	if !ok {
		return nil, "", fmt.Errorf("unknown backend %q", parts[0])
	}
	return b, parts[1], nil
}
