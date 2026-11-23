// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HandleAdmin serves the management API:
//
//	GET  /admin/backends                    list backends with state
//	GET  /admin/backends/{name}/logs        tail captured process logs
//	POST /admin/backends/{name}/start       start the managed process
//	POST /admin/backends/{name}/stop        kill the managed process
//	POST /admin/backends/{name}/unload      unload the model (koboldcpp)
//	POST /admin/backends/{name}/load        load a model, body {"model": "..."}
//	POST /admin/backends/{name}/discover    re-discover models now
func (r *Router) HandleAdmin(w http.ResponseWriter, req *http.Request) {
	if !r.authOK(req) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid or missing API key"})
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(req.URL.Path, "/admin"), "/"), "/")
	switch parts[0] {
	case "backends":
		r.handleAdminBackends(w, req, parts)
	case "jobs":
		r.handleAdminJobs(w, req, parts)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (r *Router) handleAdminBackends(w http.ResponseWriter, req *http.Request, parts []string) {
	if len(parts) == 1 {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.handleAdminList(w)
		return
	}
	name := parts[1]
	b, ok := r.backends[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown backend " + name})
		return
	}
	action := ""
	if len(parts) > 2 {
		action = parts[2]
	}

	switch {
	case req.Method == http.MethodGet && action == "":
		r.handleAdminList(w)

	case req.Method == http.MethodGet && action == "logs":
		lines := 100
		if v := req.URL.Query().Get("lines"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				lines = n
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"backend": name, "logs": b.Logs(lines)})

	case req.Method == http.MethodPost && action == "start":
		if err := b.StartProcess(req.Context()); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		b.Discover(req.Context())
		writeJSON(w, http.StatusOK, map[string]any{"backend": name, "state": b.State().String()})

	case req.Method == http.MethodPost && action == "stop":
		b.StopProcess()
		writeJSON(w, http.StatusOK, map[string]any{"backend": name, "state": b.State().String()})

	case req.Method == http.MethodPost && action == "unload":
		if err := b.FreeGPU(req.Context()); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"backend": name, "state": b.State().String()})

	case req.Method == http.MethodPost && action == "load":
		var in struct {
			Model string `json:"model"`
		}
		json.NewDecoder(req.Body).Decode(&in)
		if in.Model == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model is required"})
			return
		}
		if err := b.EnsureLoaded(req.Context(), in.Model); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"backend": name, "state": b.State().String(), "loaded": b.LoadedModel(),
		})

	case req.Method == http.MethodPost && action == "discover":
		if err := b.Discover(req.Context()); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"backend": name, "models": b.Models()})

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (r *Router) handleAdminJobs(w http.ResponseWriter, req *http.Request, parts []string) {
	if r.jobs == nil || len(r.jobs.jobs) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "no jobs configured"})
		return
	}
	if len(parts) == 1 {
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.jobs.handleList(w)
		return
	}
	name := parts[1]
	action := ""
	if len(parts) > 2 {
		action = parts[2]
	}
	switch {
	case req.Method == http.MethodGet && action == "":
		r.jobs.handleOne(w, name)
	case req.Method == http.MethodGet && action == "logs":
		r.jobs.handleLogs(w, req, name)
	case req.Method == http.MethodPost && action == "run":
		if err := r.jobs.runNow(name); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"job": name, "status": "started"})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (r *Router) handleAdminList(w http.ResponseWriter) {
	type info struct {
		Name         string   `json:"name"`
		Type         string   `json:"type"`
		State        string   `json:"state"`
		LoadedModel  string   `json:"loaded_model,omitempty"`
		Models       []string `json:"models"`
		PID          int      `json:"pid,omitempty"`
		Owned        bool     `json:"owned"`
		LastActivity string   `json:"last_activity"`
	}
	out := []info{}
	for _, name := range r.order {
		b := r.backends[name]
		m := []string{}
		for _, mm := range b.Models() {
			m = append(m, mm.Name)
		}
		out = append(out, info{
			Name:         name,
			Type:         b.Type(),
			State:        b.State().String(),
			LoadedModel:  b.LoadedModel(),
			Models:       m,
			PID:          b.PID(),
			Owned:        r.cfgs[name].Command != "",
			LastActivity: b.LastActivity().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
