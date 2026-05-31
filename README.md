# Narya Runtime Engine

```
 _   _    _    ______   __    _    
| \ | |  / \  |  _ \ \ / /   / \   
|  \| | / _ \ | |_) \ V /   / _ \  
| |\  |/ ___ \|  _ < | |   / ___ \ 
|_| \_/_/   \_\_| \_\|_|  /_/   \_\

NaryaEngineRunner — The Runtime that Ignites and Scales Applications
```

**Narya** is a high-performance runtime engine that turns traditional PHP applications into always-on servers with persistent workers. Built in **Go** with **Unix Domain Sockets** and **MessagePack** for inter-process communication, it removes the per-request PHP bootstrap overhead.

---

## Table of Contents

- [Why Narya?](#why-narya)
- [Architecture](#architecture)
- [Installation](#installation)
- [Usage](#usage)
- [Integration with PHP Applications](#integration-with-php-applications)
- [Configuration](#configuration)
- [Example API](#example-api)
- [Communication Protocol](#communication-protocol)
- [Security and Isolation](#security-and-isolation)
- [Roadmap](#roadmap)
- [Project Structure](#project-structure)
- [License](#license)

---

## Why Narya?

### The Problem

In traditional PHP applications, **every request** goes through:

1. PHP interpreter startup
2. Autoloader loading (Composer)
3. Framework bootstrap (Laravel: ~50–100ms, Symfony: ~30–80ms)
4. Configuration loading
5. DI container initialization
6. Database connection setup
7. **Finally**, request handling

This overhead repeats **thousands of times per second** in production.

### The Solution

Narya keeps **persistent PHP workers** that:

- Run bootstrap **once**
- Keep database connections **open**
- Keep the DI container **initialized**
- Handle requests with minimal per-request overhead

```
┌─────────────────────────────────────────────────────────────┐
│                      NARYA RUNTIME                           │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│   ┌─────────┐               ┌──────────────────────────┐    │
│   │ Client  │ ────────────► │     Go HTTP Server       │    │
│   └─────────┘               │  (Pool Manager, Router)   │    │
│                             └───────────┬──────────────┘    │
│                                         │                    │
│                          UDS + MessagePack                   │
│                                         │                    │
│              ┌──────────────────────────┼──────────────┐   │
│              │                          │               │   │
│              ▼                          ▼               ▼   │
│      ┌──────────────┐          ┌──────────────┐   ┌──────┐  │
│      │ PHP Worker 1 │          │ PHP Worker 2 │...│ W(n) │  │
│      │  (warm)      │          │  (warm)      │   │      │  │
│      └──────────────┘          └──────────────┘   └──────┘  │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

---

## Architecture

### Components

| Component       | Language | Responsibility |
|-----------------|----------|----------------|
| **HTTP Server** | Go       | Accepts HTTP requests, routes app traffic and `/narya/*` ops endpoints |
| **Worker Pool** | Go       | Spawns, scales, monitors, and recycles PHP workers |
| **Protocol**    | Go/PHP   | MessagePack serialization with binary framing |
| **Worker**      | PHP (SDK)| Orchestrator: UDS bridge, state reset, error handling |
| **Runtime CLI** | Go       | Start/stop server, read config, hot-update pool settings |

### Tech Stack

- **Go 1.21+**: HTTP server, concurrency, process management
- **PHP 8.0+** with `ext-msgpack`: Persistent workers, application logic
- **Unix Domain Sockets**: Low-latency inter-process communication
- **MessagePack**: Binary serialization (smaller than JSON)

---

## Installation

### Requirements

- Go 1.21+
- PHP 8.0+ with `ext-msgpack`
- Linux or WSL (UDS is not supported on native Windows)

### 1. Install MessagePack extension (PHP)

```bash
# Ubuntu/Debian
sudo apt install php-msgpack

# Or via PECL
pecl install msgpack
echo "extension=msgpack.so" >> /etc/php/8.x/cli/php.ini
```

### 2. Build the Go binary

```bash
make build

# Cross-compile for Linux
make linux
```

### 3. Configure and verify

```bash
cp nry.yaml.example nry.yaml
php -m | grep msgpack
./narya --version
```

---

## Usage

### Start the runtime

```bash
# Default config file: .nry.yaml (or nry.yaml in project root)
./narya -config=nry.yaml

# Override flags
./narya -config=nry.yaml -host=0.0.0.0 -port=8888 -workers=4
```

The server listens for **application traffic** on `/` and exposes **runtime endpoints** under `/narya/*`.

### Runtime endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /narya/health` | Pool health (503 if no workers) |
| `GET /narya/metrics` | Prometheus text metrics |
| `GET /narya/debug/workers` | Worker IDs, PIDs, state, request counts |
| `GET /narya/config` | Current runtime config (min/max, backpressure, keep_warm) |
| `PATCH /narya/config` | Hot-update runtime config (JSON body) |

### Quick test

```bash
curl -s http://localhost:8888/narya/health
curl -s http://localhost:8888/narya/debug/workers
curl -s http://localhost:8888/narya/metrics
```

### Runtime config CLI (while server is running)

```bash
narya -config=nry.yaml config get
narya -config=nry.yaml config set min_workers=2 max_workers=8 keep_warm=true
```

### Graceful shutdown

Send `SIGINT` or `SIGTERM` (Ctrl+C). The server drains HTTP connections (30s timeout) and stops the worker pool.

---

## Integration with PHP Applications

Narya's PHP side is the **`narya-php-sdk`**: use it in any project (Laravel, Slim, Symfony, plain PHP). Install via Composer, implement your application worker (or handler), and instantiate **`Worker`** (UDS bridge, reset, error handling).

The Go runtime starts each process with:

```bash
php worker.php --sock /path/to.sock --max-requests 300
```

See the [Example API](#example-api) section for a minimal worker script.

---

## Configuration

Copy [`nry.yaml.example`](nry.yaml.example) to `nry.yaml` (or `.nry.yaml`) and adjust as needed.

---

### `server` — HTTP server

| Option | Type | Default | Context |
|--------|------|---------|---------|
| **`host`** | string | `0.0.0.0` | Bind address |
| **`port`** | int | `8888` | TCP port |
| **`read_timeout`** | int (seconds) | `60` | Max time to read the request body |
| **`write_timeout`** | int (seconds) | `60` | Max time to write the response |
| **`enable_http2`** | bool | `true` | Reserved for HTTP/2 (not fully wired yet) |
| **`backlog`** | int | `4096` | TCP listen backlog hint |

---

### `workers` — PHP worker pool

| Option | Type | Default | Context |
|--------|------|---------|---------|
| **`count`** | int | `4` | Target worker count; must satisfy `min_workers <= count <= max_workers` |
| **`min_workers`** | int | same as `count` | Minimum workers when scaling down |
| **`max_workers`** | int | same as `count` | Maximum workers when scaling up |
| **`keep_warm`** | bool | `false` | If `true`, disables scale-down (workers stay hot) |
| **`scale_down_idle_secs`** | int | `0` | Idle seconds before scaling down one worker at a time |
| **`aggressive_scale_down_secs`** | int | — | Idle seconds before scaling down to `min_workers` at once |
| **`max_requests`** | int | `1000` | Recycle worker after N requests; passed to PHP as `--max-requests` |
| **`timeout`** | int (seconds) | `30` | Per-request timeout (UDS deadline) |
| **`socket_dir`** | string | `/tmp/narya` | UDS socket directory (use a project path in Docker) |
| **`warmup_stagger_ms`** | int | `0` | Ms between spawning each worker at boot (`0` = parallel) |
| **`fast_warmup`** | bool | `false` | Warm up to `max_workers` in background after `min_workers` |

#### Overflow strategy — only one may be enabled

| Option | Context |
|--------|---------|
| **`backpressure`** | Reject immediately when queue is full (503) |
| **`backpressure.enabled`** | Enable backpressure |
| **`backpressure.max_queue`** | Max queued requests (`0` = no queue) |
| **`queue_timeout`** | Wait in queue up to `timeout_ms`, then 503 |
| **`queue_timeout.enabled`** | Enable queue timeout |
| **`queue_timeout.timeout_ms`** | Max wait time in queue (ms) |

---

### `php` — Worker process

| Option | Default | Context |
|--------|---------|---------|
| **`binary`** | `php` | PHP CLI binary path |
| **`worker_script`** | `worker.php` | Entry script; receives `--sock` and `--max-requests` |

---

### `logging` — Runtime logs

| Option | Default | Context |
|--------|---------|---------|
| **`level`** | `info` | `debug`, `info`, `warn`, `error` |
| **`format`** | `text` | `text` or `json` |

---

## Example API

### Example `worker.php` using the Worker orchestrator

```php
<?php

declare(strict_types=1);

require_once __DIR__ . '/vendor/autoload.php';

use Narya\SDK\Runtime\Worker;

const HEALTH_JSON = '{"status":"ok"}';
const HEADERS_JSON = ['Content-Type' => ['application/json']];

$handler = function (array $request): array {
    $method = $request['method'] ?? 'GET';
    $path   = strtok($request['path'] ?? '/', '?') ?: '/';

    if ($path === '/health' && $method === 'GET') {
        return ['status' => 200, 'headers' => HEADERS_JSON, 'body' => HEALTH_JSON, 'error' => ''];
    }

    return [
        'status'  => 404,
        'headers' => HEADERS_JSON,
        'body'    => '{"error":"Not Found"}',
        'error'   => '',
    ];
};

try {
    (new Worker(null, $handler))->run();
} catch (Throwable $e) {
    fwrite(STDERR, "[FATAL] Worker error: {$e->getMessage()}\n");
    exit(1);
}
```

For Laravel, bootstrap the app inside a dedicated worker class and pass it to `Worker` — connect to UDS **before** heavy bootstrap when possible (see [PHP SDK alignment](#php-sdk-alignment-narya-php-sdk)).

---

## Communication Protocol

### Framing

```
┌────────────────┬─────────────────────────────┐
│ 4 bytes (BE)   │ N bytes                     │
│ Length         │ MessagePack payload         │
└────────────────┴─────────────────────────────┘
```

Handshake: Go sends `NARYA1`, PHP responds `OK`.

### Request (Go → PHP)

- `id`, `method`, `uri`, `path`, `query`, `protocol`, `headers`, `body`, `server`, `remote_addr`, `host`, `scheme`, `timeout_ms`
- **`worker_id`** (string) — PHP worker slot handling the request (traceability)
- **`runtime_version`** (string) — Go runtime version (e.g. `2.0.0`)

### Response (PHP → Go)

- `id`, `status`, `headers`, `body`, `error`, optional `_meta` (`req_count`, `mem_usage`, `mem_peak`, `recycle`)

### PHP SDK alignment (`narya-php-sdk`)

1. **`max_requests`** — Runtime passes `--max-requests` from `workers.max_requests` in `nry.yaml`; SDK should align cooperative recycle via `_meta.recycle`.
2. **Socket timeout** — Reset `stream_set_timeout` after each request (do not leave per-request `timeout_ms` permanently).
3. **Early UDS connect** — Complete handshake before heavy framework bootstrap to avoid spawn timeouts and OOM when many workers start in parallel.

---

## Security and Isolation

### State reset

Each worker **must** reset state between requests. The SDK **Worker** class orchestrates reset and error handling; application code should avoid mutable singletons and global state.

### Automatic recycling

Workers are recycled after:

- **N requests** (`max_requests`, aligned Go ↔ PHP)
- **Timeout** (`timeout`, UDS deadline)
- **Crash or fatal error** (process reaper + idempotent respawn)
- **Cooperative recycle** (`_meta.recycle` from PHP)

---

## Roadmap

### Phase 1 — Core (done)

- [x] Go HTTP server with worker pool
- [x] UDS + MessagePack protocol
- [x] Worker spawn, recycle, and dynamic scaling (`min_workers` / `max_workers`)
- [x] Backpressure and queue timeout overflow strategies
- [x] Health check (`/narya/health`)
- [x] Prometheus metrics (`/narya/metrics`)
- [x] Debug workers endpoint (`/narya/debug/workers`)
- [x] Graceful shutdown (SIGINT / SIGTERM)
- [x] Runtime hot config (`/narya/config`, `narya config get/set`)
- [x] Worker traceability (`worker_id`, `runtime_version` on every request)
- [x] `--max-requests` passed to PHP on spawn
- [x] UDS request deadlines and idempotent worker respawn
- [x] Process reaper (`exitCh`) for early dead-worker detection
- [x] Warmup stagger and `fast_warmup`
- [x] `keep_warm` mode (disable scale-down)

### Phase 2 — Stability & polish

- [ ] Full HTTP/2 support (`enable_http2` wired end-to-end)
- [ ] Hot reload (SIGHUP) — reload config without full restart
- [ ] Web dashboard for pool visualization
- [ ] Configurable worker spawn/connect timeout in `nry.yaml`

### Phase 3 — Advanced

- [ ] WebSocket support
- [ ] Server-Sent Events (SSE)
- [ ] First-class multi-framework adapters (Laravel, Symfony, Slim)
- [ ] systemd / supervisor unit templates

---

## Project Structure

```
NaryaRuntimeEngine/
├── main.go              # HTTP server, routes, graceful shutdown
├── worker.go            # Worker pool, spawn, respawn, scaling
├── protocol.go          # MessagePack framing and request pools
├── config.go            # nry.yaml schema and validation
├── runtime_config.go    # Hot runtime config (min/max, backpressure)
├── logger.go
├── version.go
├── Makefile
├── nry.yaml.example     # Example config (copy to nry.yaml)
├── tests/fixtures/      # PHP workers for integration tests
├── worker_test.go
├── worker_integration_test.go
├── protocol_test.go
├── config_test.go
├── README.md
└── LICENSE
```

PHP application code (`worker.php`, framework integration) lives in your project; this repository is the Go runtime.

---

## License

**MIT License** — see [LICENSE](LICENSE).

---

## Contributing

1. Fork the repository.
2. Create a feature branch (`git checkout -b feature/AmazingFeature`).
3. Commit your changes.
4. Push and open a Pull Request.

---

<p align="center">
  <strong>Narya</strong> — Keeping the flame alive<br>
  <em>Inspired by Tolkien's Ring of Fire</em>
</p>
