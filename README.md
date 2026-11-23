# llm-router

A small OpenAI-compatible LLM router in Go. It sits in front of one or more
engines (koboldcpp instances, vLLM, llama.cpp server, LM Studio, remote OpenAI
APIs, ...) and routes requests based on the `model` parameter.

- **Model discovery** — model lists come from the backends themselves
  (koboldcpp admin API / OpenAI `/v1/models`), not from static config. The
  currently-loaded model is listed by the name koboldcpp reports, so the
  running model is always requestable.
- **koboldcpp model management** — the router does **not** load or swap
  models; koboldcpp loads the requested model itself (when nothing is loaded)
  and does its own model routing. The router's only admin-API model action is
  *unloading* the model when idle (`/api/admin/reload_config` with
  `unload_model`), to free VRAM.
- **Process management** — any backend can be *owned*: the router spawns it,
  waits for health, restarts it on demand, and kills the whole process tree
  on idle timeout or shutdown. Omit `command` to attach to an existing
  instance instead.
- **VRAM-aware idling** — koboldcpp can *unload* its model after idle time
  (`idle_unload`; the process stays up, VRAM freed); owned backends can be
  *killed* after idle time (`idle_stop`), which is how a command backend frees
  the GPU.
- **GPU arbitration** — the local GPU is a shared resource. A backend is a GPU
  backend by locality (koboldcpp, or any `command` backend); the router keeps
  the GPU free by releasing other GPU backends before a model loads, and runs
  GPU-intensive cron jobs only while the GPU is idle.
- **Streaming** — SSE responses pass through in real time.
- **Full OpenAI surface** — the router is a transparent proxy: *any* path
  under `/v1` (or at the root, without the prefix) is routed to a backend —
  `chat/completions`, `completions`, `responses`, `embeddings`, `images`,
  `audio`, `files`, `moderations`, ... The backend decides which endpoints it
  actually supports; unsupported ones return the backend's own 404/405.

## Auth

Set `api_keys` in the config to require a key on every `/v1` and `/admin`
request (`/health` stays open). Clients send it the usual OpenAI way:

```yaml
api_keys:
  - ${ROUTER_API_KEY}
  - other-key-for-another-client
```

```python
client = OpenAI(base_url="http://127.0.0.1:4000/v1", api_key="ROUTER_API_KEY_VALUE")
```

`Authorization: Bearer <key>` and `x-api-key: <key>` are both accepted. With
no keys configured the router is open — fine on localhost, not fine exposed.
Keys are compared in constant time.

## Build

```sh
go build -o llm-router .            # native
GOOS=linux GOARCH=amd64 go build -o llm-router .   # for the Linux box
go test ./...
```

## Run

```sh
./llm-router -config config.yaml
```

Point any OpenAI client at `http://<host>:4000/v1`:

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:4000/v1", api_key="unused")
resp = client.chat.completions.create(model="qwen", messages=[...])
```

## Config

See [config.example.yaml](config.example.yaml). `${VAR}` references are
expanded from the environment.

```yaml
listen: 127.0.0.1:4000
refresh_interval: 5m     # model re-discovery period
swap_timeout: 15m        # legacy, unused (koboldcpp loads models itself)
max_body_bytes: 104857600  # request-body cap in bytes (default 100 MiB); 413 above it

backends:
  - name: kobold
    type: koboldcpp
    base_url: http://127.0.0.1:5001
    admin_key: ${KCPP_API_KEY}
    idle_unload: 10m     # unload model after idle (process stays up)
    # command: "koboldcpp --device cuda --openai -port 5001"   # <- own the process
    # idle_stop: 1h      # kill the process after idle (owned only)

  - name: vllm
    type: openai
    base_url: http://127.0.0.1:8000/v1
    command: "vllm serve /models/llama-70b --port 8000"
    start_timeout: 20m
    idle_stop: 30m     # stop the process after idle (frees the GPU)

  - name: openai-cloud
    type: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}

aliases:
  qwen: kobold/Qwen3.8-27B-Swift.kcppt

jobs:
  - name: nightly-train
    schedule: "0 2 * * *"      # cron: 02:00 daily
    command: "python train.py --epochs 1"
    workdir: /opt/train
    timeout: 6h                # max run time (0 = no limit)
    on_busy: defer             # defer (default) = wait for the GPU; skip = drop the run
    max_wait: 2h               # with defer: give up after this long (0 = wait forever)
```

### Backend options

| Option | Applies to | Meaning |
|---|---|---|
| `name`, `type`, `base_url` | all | identity; `type` is `koboldcpp` or `openai` |
| `admin_key` | koboldcpp | Bearer key for the admin + OpenAI endpoints; must be the server's `--password`/`--adminpassword` when set |
| `api_key` | openai | Bearer key sent to the backend |
| `command` | all | **set it to make the router own the process** (spawn/restart/kill); omit to attach |
| `workdir`, `env` | owned | process working dir and extra `KEY=VALUE` env vars |
| `auto_start` | owned | start on router startup (default `true`) |
| `start_timeout` | owned | max wait for the process to answer its health check (default 10m) |
| `health_path` | owned | path probed for readiness (defaults: kcpp `/api/admin/list_options`, openai `/v1/models`); any HTTP response counts |
| `idle_unload` | koboldcpp | unload the loaded model after this idle time (frees VRAM, process stays up) |
| `idle_stop` | owned | kill the process after this idle time (for a command backend, this frees the GPU) |
| `extra_models` | koboldcpp | models not reported by `list_options` (e.g. raw `.gguf`); added to the list so they can be requested |

### Model resolution

The `model` field of a request is resolved in this order:

1. `aliases` entry → `backend/model`
2. explicit `backend/model`
3. bare name, first backend (in config order) that lists it

### Routing the full OpenAI surface

The router proxies every OpenAI endpoint, not just completions. For each
request it picks a backend, then forwards the method, path, query string and
body unchanged (except the `model` field, which is rewritten to the backend's
real name):

- **Model-bearing requests** (`chat/completions`, `completions`, `responses`,
  `embeddings`, `images`, `audio`, ...) — the `model` field selects the
  backend via the resolution order above, and is rewritten before forwarding.
- **`GET /v1/models`** — aggregated across all backends.
- **`GET /v1/models/{id}`** — resolved to the backend that lists `{id}`, then
  forwarded there.
- **Model-less requests** (`files`, `batches`, `vector_stores`, ...) —
  forwarded to the first backend in config order.

Endpoints a backend does not implement (e.g. `embeddings` on a text-only
koboldcpp) pass through to the backend, which returns its own 404/405 — the
router never fakes a response.

## How koboldcpp backends work

- **Attach mode** (no `command`): the router assumes your instance is already
  running (systemd, your script, ...). It reads the model list from
  `GET /api/admin/list_options` (falling back to `/v1/models`), checks the
  loaded model via `GET /api/v1/model` (the `result` field), and loads/unloads
  with `POST /api/admin/reload_config` — the same calls `kcpp-loader.ps1` makes.
- **Owned mode** (`command` set): the router spawns the process on startup,
  captures its output (tail via the admin API), restarts it when a request
  finds it dead, and kills the process tree on `idle_stop` or shutdown.
- **The router never loads or swaps models.** Koboldcpp loads the requested
  model itself when nothing is loaded, and does its own model routing
  otherwise. The router just forwards every request to the backend.
- The loaded model is reported by its GGUF name (e.g.
  `koboldcpp/MyModel-Q4_K_M`), which is not a `.kcppt` in `list_options`. The
  router adds it to the model list (prefix stripped) so the running model can
  be requested by the name koboldcpp actually reports.
- `idle_unload` issues `reload_config` with `unload_model` after the idle
  period, freeing VRAM while keeping the process warm. The next request is
  forwarded as usual; koboldcpp reloads the model itself.

### Passwords and the protected-model sentinel

If koboldcpp runs with `--password` (or `--adminpassword`), set `admin_key`
to that password. Without it:

- `/api/admin/list_options` answers **200 with an empty body** (not 401), so
  the router falls back to `/v1/models` for discovery
- `/api/v1/model` answers the sentinel `koboldcpp/protected-model`; the
  router treats that as "a model is loaded" (so it forwards, not reloads)
  and logs a warning once — fix `admin_key` to get the real model name
  tracked and listed
- `reload_config` answers `{"success": false}`, which the router logs

## GPU arbitration

The local GPU is a single shared resource. A backend is a GPU backend by
locality: **koboldcpp always, and any backend the router owns via `command`**
(a local vLLM/Ollama/...). A remote backend (no `command`) does not touch the
local GPU. The router keeps the GPU free so only one consumer is active at a
time:

- **Before a model loads** — when a request is routed to a GPU backend, the
  router first releases every *other* active GPU backend (koboldcpp unloads its
  model; owned backends are killed) and any running job, then loads the model
  into the free GPU.
- **Before a job runs** — a scheduled job starts only when no GPU backend is
  active (`GPUIdle`).
- **On idle** — `idle_unload` (koboldcpp) and `idle_stop` (owned) free the GPU
  automatically, which is what makes jobs eligible.

How a backend is freed:

| Backend | Active when | Freed by |
|---|---|---|
| koboldcpp (owned or attached) | a model is loaded | unload the model (admin API) |
| openai, owned (vLLM, Ollama you spawn) | the process is running | stop the process tree |

An attached backend the router can't control (e.g. an Ollama you run yourself,
no `command`) is not a GPU backend: the router neither counts it as busy nor
tries to free it. To bring it under arbitration, own it — set `command` so the
router spawns and can kill the process.

## Jobs (idle GPU tasks)

Run GPU-intensive commands (training, batch generation, ...) on a cron
schedule, but only while the router's GPU is free. A job is eligible when **no
GPU-using backend is active** (no loaded model, no running local process).
Backends that don't use the local GPU — remote APIs, and attached backends the
router can't see — never block a job.

```yaml
jobs:
  - name: nightly-train
    schedule: "0 2 * * *"      # standard 5-field cron (or @daily, @hourly, ...)
    command: "python train.py --epochs 1"
    workdir: /opt/train        # optional
    env: ["CUDA_VISIBLE_DEVICES=0"]   # optional
    timeout: 6h                # optional; kill the run after this long (0 = no limit)
    on_busy: defer             # optional; defer (default) or skip
    max_wait: 2h               # optional; with defer, give up after this (0 = wait forever)
```

How a scheduled run resolves:

- **GPU idle at the scheduled time** → the job starts.
- **GPU busy, `on_busy: defer`** → the job waits for the GPU to free, then
  starts. `max_wait` caps the wait (default `0` = wait until it's free); once
  the cap passes, the run is skipped.
- **GPU busy, `on_busy: skip`** → the run is dropped.

Notes:

- **A GPU backend must be freeable for a job to ever run** — `idle_unload`
  (koboldcpp) or `idle_stop` (owned), or a manual free, is what frees the VRAM.
  With neither set, the backend stays active and the job never becomes eligible.
- **LLM traffic preempts a running job.** When a request is routed to a GPU
  backend, any running job is killed so the model can load. A preempted job is
  not resumed; schedule it to tolerate that.
- One instance per job at a time (no overlapping runs). A job missed while the
  router was down is not caught up — it runs at its next scheduled time.
- Jobs are stopped on router shutdown.
- `POST /admin/jobs/{name}/run` starts a job immediately, bypassing the idle
  check (the operator's explicit call).

### Job options

| Option | Required | Meaning |
|---|---|---|
| `name` | yes | unique job name |
| `schedule` | yes | cron expression (5 fields) or a descriptor like `@daily` |
| `command` | yes | command line to run (split on spaces, quotes honored) |
| `workdir` | no | working directory |
| `env` | no | extra `KEY=VALUE` env vars |
| `timeout` | no | max run time; the run is killed after this (0 = no limit) |
| `on_busy` | no | `defer` (default) or `skip` — behavior when the GPU is busy at the scheduled time |
| `max_wait` | no | with `defer`, how long to wait for the GPU before skipping (0 = wait forever) |

## Admin API

| Endpoint | Action |
|---|---|
| `GET /admin/backends` | all backends: state, loaded model, models, pid, owned |
| `GET /admin/backends/{name}/logs?lines=100` | tail captured process output |
| `POST /admin/backends/{name}/start` | start the managed process |
| `POST /admin/backends/{name}/stop` | kill the managed process |
| `POST /admin/backends/{name}/unload` | unload the model (koboldcpp) |
| `POST /admin/backends/{name}/load` | load a model now, body `{"model": "..."}` |
| `POST /admin/backends/{name}/discover` | re-discover models now |
| `GET /admin/jobs` | all jobs: schedule, next/last run, last exit, running |
| `GET /admin/jobs/{name}` | one job |
| `GET /admin/jobs/{name}/logs?lines=100` | tail captured job output |
| `POST /admin/jobs/{name}/run` | start the job now (bypasses the idle check) |

Backend states: `stopped`, `starting`, `ready`, `swapping`, `unloaded`,
`unhealthy`.

## systemd (Linux)

```ini
[Unit]
Description=llm-router
After=network.target

[Service]
ExecStart=/opt/llm-router/llm-router -config /opt/llm-router/config.yaml
Environment=ROUTER_API_KEY=...
Environment=KCPP_API_KEY=...
Environment=OPENAI_API_KEY=...
Restart=on-failure
User=llm

[Install]
WantedBy=multi-user.target
```

## Notes / limitations

- The router never loads or swaps models: it forwards every request and lets
  koboldcpp load/route. To change models, use koboldcpp's own tooling
  (e.g. `kcpp-loader.ps1`) or the admin `load` endpoint.
- The health check counts *any* HTTP response as "up"; a backend that answers
  but can't serve a specific model will surface that on the first request.

## License

Mozilla Public License 2.0 (MPL-2.0). See [LICENSE](LICENSE). Each source file
carries the MPL-2.0 notice; the full license text is in the `LICENSE` file.
