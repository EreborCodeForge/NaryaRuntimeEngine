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
- Handle requests in **microseconds**

```
┌─────────────────────────────────────────────────────────────┐
│                      NARYA RUNTIME                           │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│   ┌─────────┐    HTTP/2     ┌──────────────────────────┐    │
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

| Component      | Language | Responsibility                                      |
|----------------|----------|-----------------------------------------------------|
| **HTTP Server**| Go       | Accepts HTTP/1.1 and HTTP/2, manages worker pool   |
| **Worker Pool**| Go       | Spawns, monitors, and recycles PHP workers         |
| **Protocol**   | Go/PHP   | MessagePack serialization with binary framing      |
| **Worker**     | PHP   | Orchestrator: UDS bridge, state reset, error handling |
| **CLI**        | PHP   | Interface to start and configure the runtime       |

### Tech Stack

- **Go 1.21+**: HTTP server, concurrency, process management
- **PHP 8.0+**: Persistent workers, application logic
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

# Or for a specific target
make build-linux
```

### 3. Verify installation

```bash
php -m | grep msgpack
./narya --version
```

---

## Usage

### Start the runtime

```bash
# With config file
./narya -config=.rr.yaml

```

### Quick test

```bash
curl http://localhost:8888/health
```

---

## Integration with PHP Applications

Narya’s PHP side is a **library**: use it in any project (Laravel, Slim, Symfony, plain PHP). Install with `composer require narya/runtime`, implement a handler `(array $request): array`, and instantiate **`Worker`** (which implements the UDS bridge internally). The Go runtime starts each process with: `php worker.php --sock /path/to.sock`. The **Worker** class orchestrates the request loop, state reset between requests, and error handling.

See the [Example API](#example-api) section for a minimal worker script.

---

## Configuration

Copy `.rr.yaml.example` to `.rr.yaml` and adjust as needed.

---

### `server` — HTTP server

Controls where and how the Go HTTP server listens. All incoming HTTP requests hit this server before being dispatched to PHP workers over UDS.

| Option          | Type    | Default | Context |
|-----------------|---------|---------|---------|
| **`host`**      | string  | `0.0.0.0` | Bind address. Use `0.0.0.0` to accept external connections, or `127.0.0.1` to listen only on localhost (e.g. behind a reverse proxy). |
| **`port`**      | int     | `8888`  | TCP port. Must be free on the machine. |
| **`read_timeout`**  | int (seconds) | `60` | Max time the server waits for the client to send the request body. Prevents slow clients from holding connections. Increase for large uploads or slow networks. |
| **`write_timeout`** | int (seconds) | `60` | Max time the server waits to write the response to the client. Prevents slow clients from blocking. Increase if responses are large or clients are slow. |
| **`enable_http2`**  | bool   | `true`  | Enables HTTP/2 on the same port. Useful for multiplexing and better performance when clients support it. |

---

### `workers` — PHP worker pool

Controls how many PHP processes run, how they scale, when they are recycled, and how overflow (more requests than available workers) is handled.

| Option | Type | Default | Context |
|--------|------|---------|---------|
| **`count`** | int | `4` | **Initial and target** number of PHP workers. The pool keeps this many workers under normal load. Must satisfy `min_workers <= count <= max_workers`. |
| **`min_workers`** | int | same as `count` | Minimum workers kept alive when scaling down. Reduces idle processes in low traffic; workers are still available (warm). Set lower than `count` to allow scale-down. |
| **`max_workers`** | int | same as `count` | Maximum workers allowed when scaling up. Caps resource usage under spikes. Set higher than `count` to allow scale-up. |
| **`scale_down_idle_secs`** | int (seconds) | `0` (disabled) | After this many seconds with no work, the pool scales down **one worker at a time**. Use for gradual scale-down (e.g. `30`). `0` disables smooth scale-down. |
| **`aggressive_scale_down_secs`** | int (seconds) | — | After this many seconds idle, the pool scales down **all at once** to `min_workers`. Use when traffic drops for a long time (e.g. `10`) to free resources quickly. |
| **`max_requests`** | int | `1000` | After a worker handles this many requests, it is **recycled** (process restarted). Helps avoid memory leaks and long-lived state; tune per app. `0` = no limit (not recommended). |
| **`timeout`** | int (seconds) | `30` | **Per-request** timeout. If a PHP worker does not respond within this time, the request is aborted and the worker may be recycled. Increase for slow endpoints. |

#### Overflow strategy — only one may be enabled

When all workers are busy, new requests are queued (or rejected). You choose one strategy:

| Option | Type | Context |
|--------|------|---------|
| **`backpressure`** | object | **Reject immediately** when the queue is full. Use when you prefer fast failure (503) over waiting. |
| **`backpressure.enabled`** | bool | `false` | Set `true` to enable backpressure. |
| **`backpressure.max_queue`** | int | — | Max requests allowed in the queue. When exceeded, new requests get 503. `0` = no queue (reject as soon as all workers are busy). |
| **`queue_timeout`** | object | **Wait up to a limit** in the queue; if not dispatched in time, return 503. Use when you accept a short wait for availability. |
| **`queue_timeout.enabled`** | bool | `false` | Set `true` to enable queue timeout. |
| **`queue_timeout.timeout_ms`** | int (ms) | — | Max time a request may wait in the queue. After this, the client receives 503. |

**Rule:** Only one of `backpressure` and `queue_timeout` may have `enabled: true`. If both are off, the server uses default queuing behavior (no hard limit or timeout).

---

### `php` — How workers are started

Tells the Go runtime which PHP binary and script to run for each worker process. The Go process spawns: `php` with the configured worker script and passes `--sock` plus the socket path.

| Option | Type | Default | Context |
|--------|------|---------|---------|
| **`binary`** | string | `php` | Path to the PHP CLI binary. Use full path (e.g. `/usr/bin/php` or `/opt/php/8.2/bin/php`) if `php` is not in the process `PATH` when the runtime starts. |
| **`worker_script`** | string | `worker.php` | Path to the worker entry script (relative to the process CWD or absolute). This script must instantiate `Worker` and call `run()`; it receives `--sock /path/to.sock` from the runtime. |

---

### `logging` — Runtime logs

Controls the Go runtime’s own logs (startup, worker spawn/recycle, errors). Does not control PHP application logs.

| Option | Type | Default | Context |
|--------|------|---------|---------|
| **`level`** | string | `info` | Minimum level to emit: `debug`, `info`, `warn`, `error`. Use `debug` for troubleshooting pool and protocol issues. |
| **`format`** | string | `text` | Output format: `text` (human-readable lines) or `json` (one JSON object per line for log aggregators). |

---

### Environment variables

Configuration can be overridden with environment variables when supported by the binary (e.g. `NARYA_HOST`, `NARYA_PORT`, `NARYA_WORKERS`, `NARYA_LOG_LEVEL`). Check the binary’s `-help` or docs for the exact names.

---

## Example API

### Example `worker.php` using the Worker orchestrator

The Go runtime starts each PHP process with: `php worker.php --sock /path/to.sock`. Use the **Worker** class with a callable handler; it implements the UDS bridge internally and handles state reset and errors.

```php
<?php

/**
 * Example worker using the Worker class (orchestrator with reset, error handling).
 *
 * The Go runtime starts: php worker.php --sock /path/to.sock
 * Worker accepts a callable handler: same API, with state reset between requests.
 */

declare(strict_types=1);

require_once __DIR__ . '/vendor/autoload.php';

use Narya\Runtime\Worker;

// Pre-encoded responses (example; your app may use a router or framework)
const HEALTH_JSON = '{"status":"ok","service":"api-go","version":"1.0.0"}';
const ROOT_JSON   = '{"message":"Hello from Go API","data":[{"id":"1","name":"Gandalf","email":"gandalf@middleearth.com"},{"id":"2","name":"Frodo","email":"frodo@shire.com"},{"id":"3","name":"Aragorn","email":"aragorn@gondor.com"}],"total":3}';
const HEADERS_JSON = ['Content-Type' => ['application/json']];

$handler = function (array $request): array {
    $method = $request['method'] ?? 'GET';
    $path   = strtok($request['path'] ?? '/', '?') ?: '/';

    if ($path === '/health' && $method === 'GET') {
        return ['status' => 200, 'headers' => HEADERS_JSON, 'body' => HEALTH_JSON, 'error' => ''];
    }
    if ($path === '/' && $method === 'GET') {
        return ['status' => 200, 'headers' => HEADERS_JSON, 'body' => ROOT_JSON, 'error' => ''];
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

---

## Communication Protocol

### Framing

```
┌────────────────┬─────────────────────────────┐
│ 4 bytes (BE)   │ N bytes                     │
│ Length         │ MessagePack payload         │
└────────────────┴─────────────────────────────┘
```

### Request (Go → PHP)

- `id`, `method`, `uri`, `path`, `query`, `headers`, `body`, `remote_addr`, `host`, `scheme`, `timeout_ms`
- **`worker_id`** (int) — Set by the runtime on every request. Identifies which PHP worker process is handling the request. Use in the PHP SDK for logs, metrics, and distributed tracing.
- **`runtime_version`** (string) — Set by the runtime on every request. Version of the Narya Go binary (e.g. `2.0.0`). Use in the PHP SDK for traceability and compatibility checks.

### Response (PHP → Go)

- `id`, `status`, `headers`, `body`, `error`, and optional `_meta` (e.g. `req_count`, `mem_usage`, `mem_peak`, `recycle`).

---

## Security and Isolation

### State reset

Each worker **must** reset state between requests (e.g. clear DI container, `$_GET`/`$_POST`, request-scoped caches, open DB cursors). The **Worker** class orchestrates this: use `Worker` with your handler so that reset and error handling are applied between requests. You can also implement a custom reset step in your application layer if needed.

### Automatic recycling

Workers are recycled after:

- **N requests** (`max_requests`)
- **Timeout** (`timeout`)
- **Crash or fatal error**
- **Memory growth** (when tracked via `_meta`)

### Best practices

1. Avoid mutable singletons; use request-scoped services.
2. Do not rely on global variables across requests.
3. Close resources (streams, handles) after each request.
4. Implement a strict reset between requests.

---

## Roadmap

### Phase 1

- [x] Go server with worker pool
- [x] UDS + MessagePack communication
- [x] CLI to start runtime
- [x] Worker recycling and scaling
- [x] Health checks

### Phase 2 – Stability

- [ ] Full HTTP/2 support
- [ ] Prometheus metrics (`/metrics`)
- [ ] Graceful shutdown (SIGTERM)
- [ ] Hot reload (SIGHUP)
- [ ] Web dashboard

### Phase 3 – Advanced

- [ ] WebSocket support
- [ ] Server-Sent Events (SSE)
- [ ] Multi-framework support
- [ ] systemd/supervisor integration

---

## Project Structure

```
NaryaRuntimeEngine/
├── main.go           # Entry point, HTTP server
├── worker.go         # Pool manager, process lifecycle
├── protocol.go       # MessagePack serialization
├── config.go         # Configuration loading
├── logger.go         # Logging
├── go.mod
├── Makefile          # Build scripts
├── .rr.yaml.example  # Example configuration (copy to .rr.yaml)
├── README.md
└── LICENSE
```

PHP application code (e.g. `worker.php`, framework integration) lives in your project; this repository is the Go runtime and config schema.

---

## License

**MIT License** — open source and free to use, modify, and distribute. You may use it commercially; keep the copyright and license notice in the code. See [LICENSE](LICENSE) for the full text.

---

## Contributing

1. Fork the repository.
2. Create a feature branch (`git checkout -b feature/AmazingFeature`).
3. Commit your changes (`git commit -m 'Add AmazingFeature'`).
4. Push to the branch (`git push origin feature/AmazingFeature`).
5. Open a Pull Request.

---

<p align="center">
  <strong>Narya</strong> — Keeping the flame alive<br>
  <em>Inspired by Tolkien's Ring of Fire</em>
</p>
