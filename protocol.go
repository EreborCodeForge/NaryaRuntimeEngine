package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/vmihailenco/msgpack/v5"
)

const (
	MaxPayloadSize = 10 * 1024 * 1024
	MagicHandshake = "NARYA1"
	HandshakeOK    = "OK"
)

var requestIDCounter uint64

type Request struct {
	ID              uint64              `msgpack:"id"`
	Method          string              `msgpack:"method"`
	URI             string              `msgpack:"uri"`
	Path           string              `msgpack:"path"`
	Query           string              `msgpack:"query"`
	Headers         map[string][]string `msgpack:"headers"`
	Body            []byte              `msgpack:"body"`
	RemoteAddr      string              `msgpack:"remote_addr"`
	Host            string              `msgpack:"host"`
	Scheme          string              `msgpack:"scheme"`
	TimeoutMs       int                 `msgpack:"timeout_ms"`
	Meta            map[string]string   `msgpack:"meta,omitempty"`
	WorkerID        int                 `msgpack:"worker_id,omitempty"`   // set by runtime for PHP SDK traceability
	RuntimeVersion  string              `msgpack:"runtime_version,omitempty"` // set by runtime for PHP SDK traceability
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

	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))

	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("erro ao escrever payload: %w", err)
	}

	return nil
}

func (p *Protocol) ReadFrame(r io.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("failed to read header: %w", err)
	}

	size := binary.BigEndian.Uint32(header)

	if size > MaxPayloadSize {
		return nil, fmt.Errorf("payload excede limite: %d > %d", size, MaxPayloadSize)
	}

	if size == 0 {
		return nil, fmt.Errorf("empty payload")
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("erro ao ler payload: %w", err)
	}

	return payload, nil
}

func (p *Protocol) SendRequest(w io.Writer, req *Request) error {
	payload, err := msgpack.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to serialize request: %w", err)
	}
	return p.WriteFrame(w, payload)
}

func (p *Protocol) ReceiveResponse(r io.Reader) (*Response, error) {
	payload, err := p.ReadFrame(r)
	if err != nil {
		return nil, err
	}

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
	payload, err := p.ReadFrame(r)
	if err != nil {
		return nil, err
	}

	var req Request
	if err := msgpack.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("erro ao desserializar request: %w", err)
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
