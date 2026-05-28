package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/vmihailenco/msgpack/v5"
)

const defaultBufferSize = 64 * 1024

var bufferPool = sync.Pool{
	New: func() any { return make([]byte, defaultBufferSize) },
}

// Pool para headers de 4 bytes (evita alocação por request)
var headerPool = sync.Pool{
	New: func() any { return make([]byte, 4) },
}

// Pool para bytes.Buffer (serialização msgpack)
var msgpackBufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// Pool para bufio.Writer (buffered writes para UDS)
var bufWriterPool = sync.Pool{
	New: func() any { return bufio.NewWriterSize(nil, 8192) },
}

// Pool para Request structs
var requestPool = sync.Pool{
	New: func() any {
		return &Request{
			Headers: make(map[string][]string, 16),
			Server:  make(map[string]string, 24),
			Meta:    make(map[string]string, 4),
		}
	},
}

// AcquireRequest obtém um Request do pool
func AcquireRequest() *Request {
	return requestPool.Get().(*Request)
}

// ReleaseRequest devolve um Request ao pool
func ReleaseRequest(r *Request) {
	// Limpa campos para reutilização
	r.ID = 0
	r.Method = ""
	r.URI = ""
	r.Path = ""
	r.Query = ""
	r.Protocol = ""
	r.Body = r.Body[:0]
	r.RemoteAddr = ""
	r.Host = ""
	r.Scheme = ""
	r.TimeoutMs = 0
	r.WorkerID = ""
	r.RuntimeVersion = ""
	// Limpa maps sem realocar
	for k := range r.Headers {
		delete(r.Headers, k)
	}
	for k := range r.Server {
		delete(r.Server, k)
	}
	for k := range r.Meta {
		delete(r.Meta, k)
	}
	requestPool.Put(r)
}

const (
	MaxPayloadSize   = 10 * 1024 * 1024
	MagicHandshake   = "NARYA1"
	HandshakeOK      = "OK"
	RuntimeVersion   = "1.0.0"
)

var requestIDCounter uint64

type Request struct {
	ID              uint64              `msgpack:"id"`
	Method          string              `msgpack:"method"`
	URI             string              `msgpack:"uri"`   // full URL e.g. http://localhost:8888/api/users?active=1 (PSR-7)
	Path            string              `msgpack:"path"`
	Query           string              `msgpack:"query"`
	Protocol        string              `msgpack:"protocol,omitempty"` // e.g. "1.1" (PSR-7)
	Headers         map[string][]string `msgpack:"headers"`
	Body            []byte              `msgpack:"body"`
	Server          map[string]string   `msgpack:"server,omitempty"` // CGI/Apache-style env (PSR-7 bridge)
	RemoteAddr      string              `msgpack:"remote_addr"`
	Host            string              `msgpack:"host"`
	Scheme          string              `msgpack:"scheme"`
	TimeoutMs       int                 `msgpack:"timeout_ms"`
	Meta            map[string]string   `msgpack:"meta,omitempty"`
	WorkerID        string              `msgpack:"worker_id,omitempty"`
	RuntimeVersion  string              `msgpack:"runtime_version,omitempty"`
}

type Response struct {
	ID      uint64              `msgpack:"id"`
	Status  int                 `msgpack:"status"`
	Headers map[string][]string `msgpack:"headers"`
	Body    []byte              `msgpack:"body"`
	Error   string              `msgpack:"error,omitempty"`
	Meta    ResponseMeta        `msgpack:"_meta,omitempty"`
}

type ResponseMeta struct {
	ReqCount  int  `msgpack:"req_count"`
	MemUsage  int  `msgpack:"mem_usage"`
	MemPeak   int  `msgpack:"mem_peak"`
	Recycle   bool `msgpack:"recycle,omitempty"`
}

type Protocol struct{}

func NewProtocol() *Protocol {
	return &Protocol{}
}

func NextRequestID() uint64 {
	return atomic.AddUint64(&requestIDCounter, 1)
}

func (p *Protocol) WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxPayloadSize {
		return fmt.Errorf("payload exceeds max size: %d > %d", len(payload), MaxPayloadSize)
	}

	header := headerPool.Get().([]byte)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))

	if _, err := w.Write(header); err != nil {
		headerPool.Put(header)
		return fmt.Errorf("failed to write header: %w", err)
	}
	headerPool.Put(header)

	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("failed to write payload: %w", err)
	}

	return nil
}

func (p *Protocol) ReadFrame(r io.Reader) ([]byte, func(), error) {
	header := headerPool.Get().([]byte)
	if _, err := io.ReadFull(r, header); err != nil {
		headerPool.Put(header)
		if err == io.EOF {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("failed to read header: %w", err)
	}

	size := binary.BigEndian.Uint32(header)
	headerPool.Put(header)

	if size > MaxPayloadSize {
		return nil, nil, fmt.Errorf("payload exceeds limit: %d > %d", size, MaxPayloadSize)
	}

	if size == 0 {
		return nil, nil, fmt.Errorf("empty payload")
	}

	buf := bufferPool.Get().([]byte)
	if cap(buf) < int(size) {
		bufferPool.Put(buf)
		payload := make([]byte, size)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, nil, fmt.Errorf("failed to read payload: %w", err)
		}
		return payload, func() {}, nil
	}
	buf = buf[:size]
	if _, err := io.ReadFull(r, buf); err != nil {
		bufferPool.Put(buf[:cap(buf)])
		return nil, nil, fmt.Errorf("failed to read payload: %w", err)
	}
	return buf, func() { bufferPool.Put(buf[:cap(buf)]) }, nil
}

func (p *Protocol) SendRequest(w io.Writer, req *Request) error {
	buf := msgpackBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer msgpackBufferPool.Put(buf)

	enc := msgpack.NewEncoder(buf)
	if err := enc.Encode(req); err != nil {
		return fmt.Errorf("failed to serialize request: %w", err)
	}
	return p.WriteFrame(w, buf.Bytes())
}

func (p *Protocol) ReceiveResponse(r io.Reader) (*Response, error) {
	payload, release, err := p.ReadFrame(r)
	if err != nil {
		return nil, err
	}
	defer release()

	var resp Response
	if err := msgpack.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("failed to deserialize response: %w", err)
	}

	return &resp, nil
}

func (p *Protocol) SendResponse(w io.Writer, resp *Response) error {
	payload, err := msgpack.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to serialize response: %w", err)
	}
	return p.WriteFrame(w, payload)
}

func (p *Protocol) ReceiveRequest(r io.Reader) (*Request, error) {
	payload, release, err := p.ReadFrame(r)
	if err != nil {
		return nil, err
	}
	defer release()

	var req Request
	if err := msgpack.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("failed to deserialize request: %w", err)
	}

	return &req, nil
}

func (p *Protocol) Handshake(rw io.ReadWriter) error {
	magic := []byte(MagicHandshake)
	if _, err := rw.Write(magic); err != nil {
		return fmt.Errorf("failed to send handshake: %w", err)
	}

	resp := make([]byte, len(HandshakeOK))
	if _, err := io.ReadFull(rw, resp); err != nil {
		return fmt.Errorf("erro ao ler handshake response: %w", err)
	}

	if string(resp) != HandshakeOK {
		return fmt.Errorf("invalid handshake: expected %s, got %s", HandshakeOK, string(resp))
	}

	return nil
}
