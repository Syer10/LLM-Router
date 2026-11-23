# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.1.0] - 2026-11-23

### Added

- **Idle GPU job scheduler** — cron-scheduled, GPU-intensive commands that run only while the GPU is idle. Per-job `on_busy` (`defer`/`skip`) and `max_wait`; LLM traffic preempts (kills) a running job.
- **Generalized GPU arbitration** — the local GPU is now coordinated across all local backends, inferred from locality (koboldcpp always, plus any `command`/owned backend), not just koboldcpp.
- **`extra_models` for all backends** — declare a model so a command backend can be routed to / aliased before its process has started.
- **Runtime model-name rewriting** — when a resolved model isn't one the backend reports and the backend serves exactly one model, the router rewrites the request to that name.

### Changed

- Owned backends now start on demand (removed `auto_start`); no eager start at boot.
- `idle_unload` is koboldcpp-only (rejected at config load for other types); `idle_stop` frees a command backend's GPU.
- Initial model discovery runs in the background, so the server listens immediately instead of blocking on a slow/unreachable backend.

### Fixed

- koboldcpp concurrency: split the backend mutex so concurrent requests no longer serialize on admin-API calls, and an in-flight idle unload no longer stalls new requests.
- A client cancel during a slow cold-start no longer kills the shared backend process.
- `GET /v1/models/{id}` on a stopped owned backend returns a clear 503 (pointing at the admin start endpoint) instead of a bare 502.
- The `listen` address is validated at config load.

### Removed

- The `/admin/backends/{name}/load` endpoint — redundant with `/start` and unable to load a model under the pure-proxy rule.

### Tests

- Regression test: a client cancel stops the backend stream.

### Docs

- Added `AGENTS.md` with project guidance for AI agents.
- Reflowed `README.md` and `AGENTS.md` prose to natural (non-hard-wrapped) lines.

## [1.0.0] - 2026-11-22

### Added

- Initial release.
- OpenAI-compatible transparent proxy router: routes any path under `/v1` (or the root) to a backend based on the `model` field.
- Model resolution: `aliases` → `backend/model` → bare name (first backend in config order that lists it).
- Model discovery from the backends themselves (koboldcpp admin API, OpenAI `/v1/models`), not static config.
- `koboldcpp` and `openai` backends.
- Process management: own a backend's process (spawn, wait for health, restart on demand, kill the process tree) or attach to an existing instance.
- API-key auth (`Authorization: Bearer` or `x-api-key`); `/health` stays open; open when no keys are set.
- `idle_unload` to free koboldcpp VRAM after idle time.
- Streaming (SSE) passthrough and the full OpenAI surface.
- Admin API for backend state, logs, start/stop/unload, and discovery.
- CI (vet, build, test) and release workflows; MPL-2.0 license.

[1.1.0]: https://github.com/Syer10/LLM-Router/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/Syer10/LLM-Router/releases/tag/v1.0.0
