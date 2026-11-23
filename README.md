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
- **VRAM-aware idling** — koboldcpp backends can *unload* their model after
  idle time (process stays up, VRAM freed); owned processes can be *killed*
  after a longer idle time.
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
    idle_stop: 30m

  - name: openai-cloud
    type: openai
    base_url: https://api.openai.com/v1
    api_key: ${OPENAI_API_KEY}

aliases:
  qwen: kobold/Qwen3.8-27B-Swift.kcppt
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
| `idle_unload` | koboldcpp | unload the loaded model after this idle time |
| `idle_stop` | owned | kill the process after this idle time |
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
