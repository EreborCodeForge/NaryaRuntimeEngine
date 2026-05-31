Abaixo está o **spec patch técnico** para seu agente implementar no `EreborCodeForge/NaryaRuntimeEngine`, com ajustes coordenados com `narya-php-sdk`.

# Spec Patch — Worker Reliability, Timeout, Respawn e Reciclagem

## Objetivo

Corrigir degradação progressiva no runtime causada por:

1. worker PHP/Laravel persistente vivendo requests demais;
2. Runtime Go e PHP SDK com `max_requests` desalinhados;
3. worker travado sem deadline real na conexão UDS;
4. worker morto detectado tarde;
5. risco de respawn duplicado do mesmo worker;
6. buffers grandes ficando retidos em pools;
7. escrita parcial no protocolo não tratada de forma robusta.

O Runtime já instancia o `WorkerPool` com `MaxRequests`, `WorkerTimeout`, `min/max`, backpressure e queue timeout vindos da config. 
Mas hoje o processo PHP é iniciado apenas com `--sock`, sem repassar `--max-requests`. 

---

# 1. Alinhar `max_requests` entre Go Runtime e PHP SDK

## Problema

O Runtime Go tem default `workers.max_requests: 1000`. 
O PHP SDK tem default `maxRequests = 10000`. 

O SDK já sabe ler `--max-requests` via argv. 

## Patch

Arquivo: `worker.go`

Criar helper para facilitar teste:

```go
func (p *WorkerPool) buildWorkerCommand(sockPath string) *exec.Cmd {
	args := []string{
		p.workerScript,
		"--sock", sockPath,
	}

	if p.maxRequests >= 0 {
		args = append(args, "--max-requests", strconv.Itoa(p.maxRequests))
	}

	return exec.Command(p.phpBinary, args...)
}
```

Substituir em `spawnWorker`:

```go
cmd := exec.Command(p.phpBinary, p.workerScript, "--sock", sockPath)
```

por:

```go
cmd := p.buildWorkerCommand(sockPath)
```

## Critério de aceite

Com config:

```yaml
workers:
  max_requests: 300
```

o processo PHP deve iniciar como:

```bash
php worker.php --sock /tmp/narya/worker-000.sock --max-requests 300
```

---

# 2. Aplicar deadline real no socket UDS do worker

## Problema

O HTTP handler cria contexto com timeout:

```go
ctx, cancel := context.WithTimeout(r.Context(), s.config.Workers.Timeout)
resp, err := s.pool.Execute(ctx, req)
```



Mas o `Worker.Execute()` não usa esse contexto nem aplica deadline no socket. Ele apenas envia request e espera resposta. 

Se Laravel travar, o worker pode ficar ocupado indefinidamente.

## Patch

Alterar assinatura:

```go
func (w *Worker) Execute(req *Request) (*Response, error)
```

para:

```go
func (w *Worker) Execute(ctx context.Context, req *Request, timeout time.Duration) (*Response, error)
```

Implementação esperada:

```go
func (w *Worker) Execute(ctx context.Context, req *Request, timeout time.Duration) (*Response, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn == nil {
		return nil, fmt.Errorf("connection not established")
	}

	deadline := time.Now().Add(timeout)

	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	if err := w.conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("failed to set worker deadline: %w", err)
	}
	defer w.conn.SetDeadline(time.Time{})

	if err := w.protocol.SendRequest(w.conn, req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	resp, err := w.protocol.ReceiveResponse(w.conn)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("worker timeout or request cancelled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return resp, nil
}
```

Alterar chamada no pool:

```go
resp, err := worker.Execute(req)
```

para:

```go
resp, err := worker.Execute(ctx, req, p.workerTimeout)
```

## Critério de aceite

Se uma rota Laravel travar por mais que `workers.timeout`, o Runtime deve:

1. encerrar a request com erro;
2. marcar o worker como morto;
3. respawnar outro worker;
4. não deixar `busy_workers` preso.

---

# 3. Adicionar estado `WorkerStateRespawning`

## Problema

Hoje existem múltiplos caminhos de respawn:

* `validateAndPrepareWorker`;
* `ReleaseWorker`;
* `healthMonitor`.

Sem estado intermediário, dois goroutines podem tentar respawnar o mesmo worker.

## Patch

Arquivo: `worker.go`

Alterar enum:

```go
const (
	WorkerStateIdle WorkerState = iota
	WorkerStateBusy
	WorkerStateDead
	WorkerStateStarting
	WorkerStateRespawning
)
```

Atualizar `workerStateString`.

Adicionar helper:

```go
func (p *WorkerPool) markRespawning(w *Worker) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == WorkerStateRespawning {
		return false
	}

	w.state = WorkerStateRespawning
	return true
}
```

Adicionar helper centralizado:

```go
func (p *WorkerPool) scheduleRespawn(w *Worker, reason string) {
	if !p.markRespawning(w) {
		p.logger.Debug("Worker %d already respawning; reason=%s", w.ID, reason)
		return
	}

	go func() {
		newWorker, err := p.respawnWorker(w)
		if err != nil {
			p.logger.Error("Failed to respawn worker %d: %v", w.ID, err)
			p.markDead(w)
			return
		}

		select {
		case p.available <- newWorker:
			p.logger.Info("Worker %d respawned successfully; reason=%s", newWorker.ID, reason)
		case <-p.ctx.Done():
			p.destroyWorker(newWorker)
		}
	}()
}
```

Adicionar:

```go
func (p *WorkerPool) markDead(w *Worker) {
	w.mu.Lock()
	if w.state != WorkerStateRespawning {
		w.state = WorkerStateDead
	} else {
		w.state = WorkerStateDead
	}
	w.mu.Unlock()
}
```

Substituir respawns manuais por `scheduleRespawn`.

---

# 4. Reorganizar `ReleaseWorker`

## Problema

Hoje o worker é marcado como idle antes de decidir recycle. 

Isso cria janela ruim: worker que deveria morrer pode voltar para o canal `available`.

## Patch

Reescrever fluxo de `ReleaseWorker`:

```go
func (p *WorkerPool) ReleaseWorker(worker *Worker, resp *Response) {
	if atomic.AddInt32(&p.busyWorkers, -1) < 0 {
		atomic.StoreInt32(&p.busyWorkers, 0)
	}

	worker.mu.Lock()
	worker.requestCount++
	count := worker.requestCount
	currentState := worker.state

	shouldRecycle := false

	if currentState == WorkerStateDead {
		worker.mu.Unlock()
		p.scheduleRespawn(worker, "dead_on_release")
		return
	}

	if resp != nil && resp.Meta.Recycle {
		shouldRecycle = true
		p.logger.Info("Worker %d requested cooperative recycle", worker.ID)
	}

	if p.maxRequests > 0 && count >= int64(p.maxRequests) {
		shouldRecycle = true
		p.logger.Info("Worker %d reached request limit (%d)", worker.ID, p.maxRequests)
	}

	if shouldRecycle {
		worker.state = WorkerStateRespawning
		worker.mu.Unlock()
		p.scheduleRespawn(worker, "max_requests_or_cooperative_recycle")
		return
	}

	worker.state = WorkerStateIdle
	worker.mu.Unlock()

	p.lastBusyTimeMu.Lock()
	p.lastBusyTime = time.Now()
	p.lastBusyTimeMu.Unlock()

	atomic.AddInt64(&p.totalRequests, 1)

	select {
	case p.available <- worker:
	case <-p.ctx.Done():
		p.destroyWorker(worker)
	}
}
```

---

# 5. Adicionar process reaper com `cmd.Wait()`

## Problema

O health monitor detecta morte usando `ProcessState.Exited()`. 

Mas `ProcessState` só fica confiável depois de `Wait()`. Se ninguém espera o processo, o Runtime pode só descobrir a morte quando tentar usar o socket.

## Patch

Adicionar campos no `Worker`:

```go
exitCh  chan error
waitOnce sync.Once
```

No `spawnWorker`, ao criar `worker`:

```go
worker := &Worker{
	ID:             id,
	Pid:            pid,
	cmd:            cmd,
	sockPath:       sockPath,
	listener:       listener,
	state:          WorkerStateStarting,
	startTime:      time.Now(),
	lastActiveTime: time.Now(),
	protocol:       p.protocol,
	exitCh:         make(chan error, 1),
}
```

Adicionar método:

```go
func (w *Worker) startReaper(logger *Logger) {
	w.waitOnce.Do(func() {
		go func() {
			err := w.cmd.Wait()

			w.mu.Lock()
			if w.state != WorkerStateRespawning {
				w.state = WorkerStateDead
			}
			if w.conn != nil {
				_ = w.conn.Close()
			}
			if w.listener != nil {
				_ = w.listener.Close()
			}
			w.mu.Unlock()

			select {
			case w.exitCh <- err:
			default:
			}

			if err != nil && logger != nil {
				logger.Warn("Worker %d process exited: %v", w.ID, err)
			}
		}()
	})
}
```

Chamar após `cmd.Start()` e criação do struct:

```go
worker.startReaper(p.logger)
```

## Importante

Depois disso, **remover chamadas diretas repetidas a `cmd.Wait()`** dentro de `respawnWorker()` e `destroyWorker()`. Usar `exitCh`.

---

# 6. Centralizar finalização segura do worker

## Patch

Adicionar helper:

```go
func (p *WorkerPool) terminateWorker(w *Worker, reason string) {
	if w.conn != nil {
		_ = w.conn.Close()
	}

	if w.listener != nil {
		_ = w.listener.Close()
	}

	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Signal(syscall.SIGTERM)

		select {
		case <-w.exitCh:
		case <-time.After(3 * time.Second):
			_ = w.cmd.Process.Kill()

			select {
			case <-w.exitCh:
			case <-time.After(2 * time.Second):
				p.logger.Warn("Worker %d did not exit after SIGKILL; reason=%s", w.ID, reason)
			}
		}
	}

	_ = os.Remove(w.sockPath)
}
```

Atualizar:

```go
respawnWorker()
destroyWorker()
removeWorker()
Stop()
```

para usar `terminateWorker`.

---

# 7. Corrigir `respawnWorker` para não deixar slot morto preso

## Problema

Se `spawnWorker` falha durante respawn, o pool pode restaurar contador e manter worker morto no slice, degradando o pool.

## Patch esperado

`respawnWorker` deve:

1. finalizar o worker antigo;
2. remover socket antigo;
3. decrementar `activeWorkers`;
4. tentar spawn;
5. se spawn falhar, remover o worker antigo da lista;
6. não contar worker morto como ativo;
7. permitir `addWorker()` posterior recompor mínimo.

Pseudo:

```go
func (p *WorkerPool) respawnWorker(old *Worker) (*Worker, error) {
	p.terminateWorker(old, "respawn")

	atomic.AddInt32(&p.activeWorkers, -1)

	newWorker, err := p.spawnWorker(old.ID)
	if err != nil {
		p.removeWorkerFromSlice(old.ID)
		return nil, err
	}

	p.mu.Lock()
	replaced := false
	for i, w := range p.workers {
		if w.ID == old.ID {
			p.workers[i] = newWorker
			replaced = true
			break
		}
	}
	if !replaced {
		p.workers = append(p.workers, newWorker)
	}
	p.mu.Unlock()

	atomic.AddInt32(&p.activeWorkers, 1)

	return newWorker, nil
}
```

Adicionar:

```go
func (p *WorkerPool) removeWorkerFromSlice(id int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, w := range p.workers {
		if w.ID == id {
			p.workers = append(p.workers[:i], p.workers[i+1:]...)
			return
		}
	}
}
```

---

# 8. Health monitor deve garantir mínimo de workers

## Problema

Se respawn falhar, o pool pode ficar abaixo de `min_workers`.

## Patch

No final de `checkAndRespawnDeadWorkers()`:

```go
p.ensureMinWorkers()
```

Adicionar:

```go
func (p *WorkerPool) ensureMinWorkers() {
	min := p.runtime.GetMinWorkers()

	p.mu.RLock()
	current := len(p.workers)
	p.mu.RUnlock()

	for current < min {
		go p.addWorker()
		current++
	}
}
```

Também chamar `ensureMinWorkers()` no `scalerLoop`.

---

# 9. Worker inativo/morto deve ser detectado sem request

## Patch

Adicionar verificação por `exitCh` no health monitor:

```go
func (p *WorkerPool) isWorkerDead(w *Worker) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == WorkerStateDead {
		return true
	}

	select {
	case <-w.exitCh:
		w.state = WorkerStateDead
		return true
	default:
	}

	if w.cmd == nil || w.cmd.Process == nil {
		w.state = WorkerStateDead
		return true
	}

	if err := w.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		w.state = WorkerStateDead
		return true
	}

	return false
}
```

Atualizar `checkAndRespawnDeadWorkers()` para usar `isWorkerDead`.

---

# 10. Corrigir escrita parcial no protocolo

## Problema

`WriteFrame` faz `w.Write(header)` e `w.Write(payload)` uma vez. 

Para `net.Conn`, normalmente escreve tudo, mas o contrato de `io.Writer` permite escrita parcial com erro. Para robustez, usar `writeFull`.

## Patch

Arquivo: `protocol.go`

Adicionar:

```go
func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}

	return nil
}
```

Alterar `WriteFrame`:

```go
if err := writeFull(w, header); err != nil {
	headerPool.Put(header)
	return fmt.Errorf("failed to write header: %w", err)
}
headerPool.Put(header)

if err := writeFull(w, payload); err != nil {
	return fmt.Errorf("failed to write payload: %w", err)
}
```

---

# 11. Evitar retenção de buffers grandes

## Problema

`ReleaseRequest` limpa body com:

```go
r.Body = r.Body[:0]
```



Se passou um body de 10MB, esse slice pode ficar retido no pool.

## Patch

Arquivo: `protocol.go`

Adicionar constantes:

```go
const (
	MaxReusableBodyCap      = 256 * 1024
	MaxReusableMsgpackCap   = 256 * 1024
	MaxReusableHeadersCount = 128
	MaxReusableServerCount  = 128
)
```

Atualizar `ReleaseRequest`:

```go
if cap(r.Body) > MaxReusableBodyCap {
	r.Body = nil
} else {
	r.Body = r.Body[:0]
}

if len(r.Headers) > MaxReusableHeadersCount {
	r.Headers = make(map[string][]string, 16)
} else {
	for k := range r.Headers {
		delete(r.Headers, k)
	}
}

if len(r.Server) > MaxReusableServerCount {
	r.Server = make(map[string]string, 24)
} else {
	for k := range r.Server {
		delete(r.Server, k)
	}
}

for k := range r.Meta {
	delete(r.Meta, k)
}
```

Atualizar `SendRequest`:

```go
func (p *Protocol) SendRequest(w io.Writer, req *Request) error {
	buf := msgpackBufferPool.Get().(*bytes.Buffer)
	buf.Reset()

	enc := msgpack.NewEncoder(buf)
	if err := enc.Encode(req); err != nil {
		if buf.Cap() <= MaxReusableMsgpackCap {
			msgpackBufferPool.Put(buf)
		}
		return fmt.Errorf("failed to serialize request: %w", err)
	}

	err := p.WriteFrame(w, buf.Bytes())

	if buf.Cap() <= MaxReusableMsgpackCap {
		buf.Reset()
		msgpackBufferPool.Put(buf)
	}

	return err
}
```

---

# 12. Corrigir scale-down agressivo com label

## Problema

No scale-down agressivo existe `break` dentro do `select`. 

Em Go, `break` pode sair só do `select`, não do `for`, dependendo do contexto. Tornar explícito.

## Patch

```go
aggressive:
for {
	p.mu.RLock()
	current := len(p.workers)
	p.mu.RUnlock()

	if current <= min {
		break aggressive
	}

	select {
	case w := <-p.available:
		if w == nil {
			return
		}

		w.mu.Lock()
		state := w.state
		w.mu.Unlock()

		if state == WorkerStateIdle {
			p.removeWorker(w)
		} else {
			select {
			case p.available <- w:
			default:
			}
		}

	case <-time.After(100 * time.Millisecond):
		break aggressive
	}
}
```

---

# 13. Ajuste recomendado no `narya-php-sdk`

O SDK já marca `_meta.recycle` quando atinge limite. O Runtime já usa isso para reciclar worker.  

Mas revisar o `WorkerBridge` PHP:

## Patch desejado

No SDK PHP, após cada request, resetar timeout do socket para default se `timeout_ms` não vier.

Hoje o timeout muda quando request tem `timeout_ms`, mas não volta. 

Patch:

```php
$timeoutSec = 30;

if (isset($request['timeout_ms']) && $request['timeout_ms'] > 0) {
    $timeoutSec = max(1, (int) ceil($request['timeout_ms'] / 1000));
}

stream_set_timeout($this->socket, $timeoutSec);
```

---

# 14. Config recomendada para Laravel

Para diagnóstico:

```yaml
server:
  host: 0.0.0.0
  port: 8888
  read_timeout: 60
  write_timeout: 60
  backlog: 4096

workers:
  count: 4
  min_workers: 2
  max_workers: 6
  keep_warm: true

  max_requests: 100
  timeout: 30

  warmup_stagger_ms: 300
  fast_warmup: true

  backpressure:
    enabled: true
    max_queue: 100

  queue_timeout:
    enabled: true
    timeout_ms: 1000

php:
  binary: php
  worker_script: worker.php

logging:
  level: debug
  format: text
```

Depois de estabilizar:

```yaml
workers:
  count: 4
  min_workers: 2
  max_workers: 8
  keep_warm: true
  max_requests: 300
  timeout: 30

  backpressure:
    enabled: true
    max_queue: 500

  queue_timeout:
    enabled: true
    timeout_ms: 1500
```

---

# 15. Testes obrigatórios

O agente deve criar testes e provar os cenários abaixo.

## Testes unitários Go

### `TestBuildWorkerCommandIncludesMaxRequests`

Validar que `spawnWorker`/helper gera:

```bash
php worker.php --sock /tmp/narya/worker-000.sock --max-requests 300
```

### `TestWorkerExecuteUsesDeadline`

Usar `net.Pipe()`.

Cenário:

1. criar worker com `conn`;
2. chamar `Execute(ctx, req, 20ms)`;
3. não responder do outro lado;
4. assert: retorna erro de timeout;
5. duração menor que 200ms.

### `TestScheduleRespawnIsIdempotent`

Cenário:

1. worker em `Dead`;
2. chamar `scheduleRespawn` 5 vezes em paralelo;
3. garantir que só um respawn real foi iniciado.

Se precisar, criar interface/fake para `spawnWorker`.

### `TestWriteFrameHandlesPartialWriter`

Criar writer fake que escreve poucos bytes por chamada.

Assert:

1. `WriteFrame` chama `writeFull`;
2. payload final está íntegro;
3. não retorna `short write`.

### `TestReleaseRequestDropsLargeBodyBuffer`

Separar helper testável:

```go
func resetRequestForPool(r *Request)
```

Testar:

1. `Body` com cap > `MaxReusableBodyCap`;
2. após reset, `Body == nil`;
3. headers grandes recriam map;
4. headers pequenos só limpam.

### `TestAggressiveScaleDownBreaksWhenNoWorkerAvailable`

Validar que o loop agressivo não fica preso quando:

1. `len(workers) > min`;
2. canal `available` vazio;
3. timeout de 100ms acontece.

---

# 16. Testes de integração obrigatórios

Criar fixtures em `tests/fixtures`.

## Fixture 1 — worker que morre após primeira request

Arquivo:

```php
<?php
// tests/fixtures/worker_exit_after_one.php
```

Comportamento:

1. primeira request retorna 200;
2. depois encerra processo;
3. Runtime deve respawnar;
4. segunda request deve retornar 200 com novo PID.

Critério:

```bash
go test -run TestWorkerRespawnAfterExit -v
```

## Fixture 2 — worker que trava

```php
<?php
// tests/fixtures/worker_sleep.php
sleep(999);
```

Critério:

1. Runtime retorna 503/timeout;
2. worker antigo é morto;
3. novo worker aparece em `/narya/debug/workers`;
4. próximo request simples funciona.

## Fixture 3 — worker que recicla por `max_requests`

Config:

```yaml
workers:
  count: 1
  min_workers: 1
  max_workers: 1
  max_requests: 3
```

Critério:

1. fazer 5 requests;
2. PID deve mudar após a terceira;
3. nenhum request deve ficar pendurado.

---

# 17. Testes manuais de comprovação

O agente deve anexar saída destes comandos no PR.

## Build e testes

```bash
go test ./...
go test -race ./...
go test -run TestWorker -v
```

## Smoke test

```bash
./narya -config=.nry.yaml
```

Em outro terminal:

```bash
curl -s http://127.0.0.1:8888/narya/health
curl -s http://127.0.0.1:8888/narya/debug/workers
curl -s http://127.0.0.1:8888/narya/metrics
```

## Teste de respawn manual

1. pegar PID:

```bash
curl -s http://127.0.0.1:8888/narya/debug/workers
```

2. matar um worker PHP:

```bash
kill -9 <PID>
```

3. confirmar respawn em até 4 segundos:

```bash
watch -n 1 'curl -s http://127.0.0.1:8888/narya/debug/workers'
```

Critério:

* worker morto desaparece ou muda PID;
* `active_workers` volta ao mínimo;
* `available_workers` não fica zerado permanentemente.

## Teste de degradação

Rodar:

```bash
for i in $(seq 1 1000); do
  curl -s http://127.0.0.1:8888/health > /dev/null
  if [ $((i % 50)) -eq 0 ]; then
    curl -s http://127.0.0.1:8888/narya/debug/workers
  fi
done
```

Critério:

* nenhum worker fica eternamente `busy`;
* PIDs reciclam conforme `max_requests`;
* `active_workers` e `available_workers` estabilizam;
* sem crescimento indefinido de workers;
* sem socket `.sock` órfão acumulando.

---

# 18. Critério final de aceite do PR

O PR só deve ser aceito se provar:

1. Go repassa `--max-requests` para o PHP.
2. Worker travado sofre timeout real via `SetDeadline`.
3. Worker morto em idle é detectado sem precisar receber nova request.
4. Respawn não duplica worker.
5. `activeWorkers`, `workers[]` e `available` não ficam inconsistentes.
6. Buffer grande não fica retido no pool.
7. `go test -race ./...` passa.
8. Testes de integração provam:

   * morte;
   * timeout;
   * recycle por request count;
   * recuperação do pool.

O patch mais importante é **deadline no socket + reaper + respawn idempotente**. Sem isso, worker Laravel travado ainda pode degradar o pool com o tempo.
