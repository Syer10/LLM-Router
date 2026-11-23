// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen          string            `yaml:"listen"`
	Backends        []BackendConfig   `yaml:"backends"`
	Aliases         map[string]string `yaml:"aliases"`
	// If non-empty, every /v1 and /admin request must present one of these
	// keys (Authorization: Bearer <key> or x-api-key: <key>). /health stays open.
	APIKeys         []string          `yaml:"api_keys"`
	RefreshInterval time.Duration     `yaml:"refresh_interval"` // model re-discovery period
	SwapTimeout     time.Duration     `yaml:"swap_timeout"`
	// MaxBodyBytes caps the size of a single request body (memory-exhaustion
	// DoS guard). Defaults to 100 MiB when unset.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// Jobs are GPU-intensive commands run on a cron schedule, only while the
	// GPU is free (no koboldcpp model loaded).
	Jobs []Job `yaml:"jobs"`
}

type BackendConfig struct {
	Name  string `yaml:"name"`
	Type  string `yaml:"type"` // "koboldcpp" | "openai"
	BaseURL string `yaml:"base_url"`

	// koboldcpp: key for the admin API (and the OpenAI endpoint, if protected)
	AdminKey string `yaml:"admin_key"`
	// openai: API key sent to the backend
	APIKey string `yaml:"api_key"`

	// If set, the router owns this backend's process: it spawns it on
	// startup (when auto_start is not false), restarts it on demand, and
	// kills it on idle_stop / shutdown. Omit to attach to an existing instance.
	Command      string   `yaml:"command"`
	WorkDir      string   `yaml:"workdir"`
	Env          []string `yaml:"env"`
	HealthPath   string   `yaml:"health_path"`
	AutoStart    *bool    `yaml:"auto_start"`
	StartTimeout time.Duration `yaml:"start_timeout"`

	// koboldcpp: unload the loaded model after this much idle time (process stays up)
	IdleUnload time.Duration `yaml:"idle_unload"`
	// owned processes: kill the process after this much idle time
	IdleStop time.Duration `yaml:"idle_stop"`

	// Extra models not listed by the backend (e.g. raw .gguf files that
	// koboldcpp loads with a .kcpps template override).
	ExtraModels []ExtraModel `yaml:"extra_models"`
}

func (b BackendConfig) AutoStartEnabled() bool {
	return b.AutoStart == nil || *b.AutoStart
}

type ExtraModel struct {
	Name     string `yaml:"name"`
	Template string `yaml:"template"` // koboldcpp overrideconfig
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	// Expand ${VAR} and $VAR references from the environment in every
	// field (os.ExpandEnv semantics). Pass secret values like API keys via
	// environment variables, not inline in the config, since a literal $
	// in the file would be treated as a variable reference.
	data = []byte(os.Expand(string(data), os.Getenv))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:4000"
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 5 * time.Minute
	}
	if cfg.SwapTimeout == 0 {
		cfg.SwapTimeout = 15 * time.Minute
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}

	seen := map[string]bool{}
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.Name == "" {
			return nil, fmt.Errorf("backend %d: name is required", i)
		}
		if seen[b.Name] {
			return nil, fmt.Errorf("duplicate backend name %q", b.Name)
		}
		seen[b.Name] = true
		if b.Type != "koboldcpp" && b.Type != "openai" {
			return nil, fmt.Errorf("backend %q: type must be \"koboldcpp\" or \"openai\"", b.Name)
		}
		if b.BaseURL == "" {
			return nil, fmt.Errorf("backend %q: base_url is required", b.Name)
		}
		b.BaseURL = strings.TrimRight(b.BaseURL, "/")
		if b.Command != "" && b.StartTimeout == 0 {
			b.StartTimeout = 10 * time.Minute
		}
		if b.HealthPath == "" {
			if b.Type == "koboldcpp" {
				// Works even when no model is loaded.
				b.HealthPath = "/api/admin/list_options"
			} else {
				b.HealthPath = "/v1/models"
			}
		}
	}

	seenJobs := map[string]bool{}
	for i := range cfg.Jobs {
		j := &cfg.Jobs[i]
		if j.Name == "" {
			return nil, fmt.Errorf("job %d: name is required", i)
		}
		if seenJobs[j.Name] {
			return nil, fmt.Errorf("duplicate job name %q", j.Name)
		}
		seenJobs[j.Name] = true
		if j.Command == "" {
			return nil, fmt.Errorf("job %q: command is required", j.Name)
		}
		if _, err := cron.ParseStandard(j.Schedule); err != nil {
			return nil, fmt.Errorf("job %q: invalid schedule %q", j.Name, j.Schedule)
		}
		if j.OnBusy != "" && j.OnBusy != "defer" && j.OnBusy != "skip" {
			return nil, fmt.Errorf("job %q: on_busy must be \"defer\" or \"skip\"", j.Name)
		}
	}
	return &cfg, nil
}
