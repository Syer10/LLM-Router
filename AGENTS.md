# AGENTS.md

Guidance for AI agents working in this repository.

`llm-router` is a small OpenAI-compatible LLM router in Go. It sits in front of
one or more engines (koboldcpp, vLLM, llama.cpp server, LM Studio, remote OpenAI
APIs) and forwards every request to the right one based on the `model` field.
Single `main` package; the only external dependency is `gopkg.in/yaml.v3`.

## The one rule: it is a pure proxy

The router **never loads or swaps models**. Koboldcpp loads the requested model
itself (when nothing is loaded) and does its own model routing. The router's only
admin-API model action is *unloading* the model when idle (`idle_unload`) to free
VRAM. `EnsureLoaded` only starts the managed process (when owned) and then
forwards — it does not load, swap, or poll. If a change tempts you to make the
router "manage" models, stop: that is the backend's job.

## How routing works

- **Resolution order** for the `model` field: an `aliases` entry → an explicit
  `backend/model` → a bare name (the first backend in config order that lists it).
- **Transparent pass-through**: `HandleV1` routes *any* path under `/v1` (or at
  the root, no prefix) to a backend. Method, path, query, and headers are
  preserved; the backend returns its own 404/405 for endpoints it does not
  implement. The router never fakes a response.
- **`GET /v1/models`** is aggregated across all backends; **`GET /v1/models/{id}`**
  resolves to the backend that lists `{id}`. Model-less requests go to the first
  backend in config order.
- **`Forward` takes the HTTP method** so body-less requests (GET, DELETE) work,
  not just POST.

## GPU arbitration and idle jobs

The local GPU is a shared resource. A backend is a GPU backend by locality
(`BackendConfig.UsesGPU`: `Type == "koboldcpp" || Command != ""`) — koboldcpp
always, and any backend the router owns via `command`. A remote backend (no
`command`) does not touch the local GPU. The router keeps the GPU free so only
one consumer is active at a time:

- **`Router.GPUIdle`** = no GPU backend is `GPUActive`. A backend is active when
  it holds VRAM: koboldcpp with a loaded model, an owned openai with a running
  process.
- **Before a model loads**, the proxy releases every other active GPU backend
  (`Router.freeOtherGPU`) and any running job, so the model loads into a free
  GPU. `FreeGPU` is per-type: koboldcpp unloads, owned openai stops the process.
- **Idle timeouts** (`lifecycle.go`): `idle_unload` is koboldcpp-only (unloads
  the model, keeps the process); `idle_stop` kills an owned process — for a
  command backend that is how the GPU is freed. `idle_unload` is rejected at
  config load for non-kobold backends.
- **`jobs:`** run GPU-intensive commands on a cron schedule, only while
  `GPUIdle`. The scheduler (`jobs.go`) ticks every 15s; `on_busy` is `defer`
  (bounded by `max_wait`) or `skip`. Jobs are the one non-backend process the
  router spawns — they reuse `splitCommand`, `procGroupAttr`, `logBuffer` from
  `process.go`.

- **Jobs need the GPU to be freeable to ever run** — `idle_unload`/`idle_stop`
  (or a manual free) is what frees the VRAM.
- **A preempted job is not resumed**: LLM traffic (and any GPU model load)
  kills a running job first.

## Gotchas (reasons behind non-obvious code)

- **The loaded-model prefix is stripped.** Koboldcpp reports the loaded model by
  its GGUF name; the router adds it to the model list with the `koboldcpp/`
  prefix *removed*, because that prefix would collide with the `backend/model`
  syntax.
- **Config env expansion is `os.Expand(string(data), os.Getenv)`.** The mapping
  func receives the variable name *without* braces; a custom mapping that checks
  for `{` breaks expansion (this was a real 401 bug). Secrets belong in env vars,
  not inline — a literal `$` in the config file is treated as a variable
  reference and gets mangled.
- **Retry-once on transient connection errors.** `forwardWithRetry` re-sends once
  on stale keep-alive failures (`server closed idle connection`, `broken pipe`,
  `connection reset by peer`, `EOF`). This is safe only because a non-nil
  `Forward` error means no response was received and the body is already in
  memory. Non-connection errors are not retried.
- **Backend auth headers are set *after* client headers are copied**, so a client
  cannot spoof the backend's `Authorization`. Key comparison is constant-time
  (`crypto/subtle`).
- **Process ownership is gated on `command`.** `command` set = owned (the router
  spawns, restarts, and kills the process tree); omitted = attach to an existing
  instance.

## Build, test, deploy

```sh
go vet ./...
go test -count=1 ./...
go build -o llm-router.exe .                          # native (Windows dev)
GOOS=linux GOARCH=amd64 go build -o llm-router-linux .  # production target
```

Production runs on **Linux** (Docker); development happens on **Windows**, so the
Linux `amd64` binary is the deployable. Production topology: client → Caddy (TLS)
→ router (`0.0.0.0:5002`) → local koboldcpp (`127.0.0.1:5001`).

## Conventions

- **Comments are purposeful.** A comment explains what the code cannot show on its
  own — the why, the gotcha, the non-obvious constraint. It does not narrate the
  change ("fixed the 401 bug", "changed X to Y"); that belongs in the commit
  message. If a comment only restates what the code already says, leave it out.
- **New `.go` files need the MPL-2.0 header** — copy the top of any existing file.
  The project is licensed MPL-2.0 (see `LICENSE`).
- **`test-live.yaml` is gitignored and points at a live server** (uses
  `${KCPP_API_KEY}`). It is for manual testing only; keep it out of commits and
  be aware requests through it hit a real endpoint.
- **Config schema lives in `config.example.yaml` and `README.md`** — update both
  when you add a config field. The full endpoint-routing and koboldcpp-admin-API
  behavior is documented in `README.md`.
- **CI** (`.github/workflows/ci.yml`) runs vet + build + test on push/PR.
  **Release** (`.github/workflows/release.yml`) builds cross-platform binaries and
  publishes a GitHub Release on a `v*` tag. In the release build, `-trimpath` is a
  `go build` flag — keep it on the build line, not in `-ldflags`.
