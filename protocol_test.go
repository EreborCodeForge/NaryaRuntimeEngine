package main

import (
	"bytes"
	"testing"
)

func TestProtocolWriteReadFrame(t *testing.T) {
	protocol := NewProtocol()

	original := []byte("Hello, MsgPack World!")

	var buf bytes.Buffer
	if err := protocol.WriteFrame(&buf, original); err != nil {
		t.Fatalf("Erro ao escrever frame: %v", err)
	}

	if buf.Len() != 4+len(original) {
		t.Fatalf("Wrong size: expected %d, got %d", 4+len(original), buf.Len())
	}

	result, err := protocol.ReadFrame(&buf)
	if err != nil {
		t.Fatalf("Erro ao ler frame: %v", err)
	}

	if !bytes.Equal(result, original) {
		t.Errorf("Content mismatch: expected %v, got %v", original, result)
	}
}

func TestProtocolRequestResponse(t *testing.T) {
	protocol := NewProtocol()

	req := &Request{
		ID:     NextRequestID(),
		Method: "POST",
		Path:   "/api/users",
		URI:    "/api/users?page=1",
		Query:  "page=1",
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"Accept":       {"application/json"},
		},
		Body:       []byte(`{"name":"John","email":"john@example.com"}`),
		RemoteAddr: "127.0.0.1:54321",
		Host:       "localhost:8080",
		Scheme:     "http",
		TimeoutMs:  5000,
	}

	var buf bytes.Buffer
	if err := protocol.SendRequest(&buf, req); err != nil {
		t.Fatalf("Erro ao enviar request: %v", err)
	}

	readReq, err := protocol.ReceiveRequest(&buf)
	if err != nil {
		t.Fatalf("Erro ao ler request: %v", err)
	}

	if readReq.ID != req.ID {
		t.Errorf("ID: expected %d, got %d", req.ID, readReq.ID)
	}
	if readReq.Method != req.Method {
		t.Errorf("Method: esperado %s, obtido %s", req.Method, readReq.Method)
	}
	if readReq.Path != req.Path {
		t.Errorf("Path: expected %s, got %s", req.Path, readReq.Path)
	}
	if !bytes.Equal(readReq.Body, req.Body) {
		t.Errorf("Body: expected %s, got %s", req.Body, readReq.Body)
	}
}

func TestProtocolResponse(t *testing.T) {
	protocol := NewProtocol()

	resp := &Response{
		ID:     123,
		Status: 201,
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"X-Request-Id": {"req-123"},
		},
		Body:  []byte(`{"id":1,"created":true}`),
		Error: "",
		Meta: ResponseMeta{
			ReqCount: 42,
			MemUsage: 1024000,
			MemPeak:  2048000,
			Recycle:  false,
		},
	}

	var buf bytes.Buffer
	if err := protocol.SendResponse(&buf, resp); err != nil {
		t.Fatalf("Failed to write response: %v", err)
	}

	readResp, err := protocol.ReceiveResponse(&buf)
	if err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	if readResp.ID != resp.ID {
		t.Errorf("ID: expected %d, got %d", resp.ID, readResp.ID)
	}
	if readResp.Status != resp.Status {
		t.Errorf("Status: expected %d, got %d", resp.Status, readResp.Status)
	}
	if !bytes.Equal(readResp.Body, resp.Body) {
		t.Errorf("Body: expected %s, got %s", resp.Body, readResp.Body)
	}
	if readResp.Meta.ReqCount != resp.Meta.ReqCount {
		t.Errorf("Meta.ReqCount: expected %d, got %d", resp.Meta.ReqCount, readResp.Meta.ReqCount)
	}
}

func TestProtocolEmptyBody(t *testing.T) {
	protocol := NewProtocol()

	req := &Request{
		ID:      1,
		Method:  "GET",
		Path:    "/",
		Headers: map[string][]string{},
		Body:    nil,
	}

	var buf bytes.Buffer
	if err := protocol.SendRequest(&buf, req); err != nil {
		t.Fatalf("Erro ao escrever request vazio: %v", err)
	}

	readReq, err := protocol.ReceiveRequest(&buf)
	if err != nil {
		t.Fatalf("Erro ao ler request vazio: %v", err)
	}

	if readReq.Method != "GET" {
		t.Errorf("Method: expected GET, got %s", readReq.Method)
	}
}

func TestProtocolErrorResponse(t *testing.T) {
	protocol := NewProtocol()

	resp := &Response{
		ID:      999,
		Status:  500,
		Headers: map[string][]string{},
		Body:    nil,
		Error:   "Internal Server Error: database connection failed",
	}

	var buf bytes.Buffer
	if err := protocol.SendResponse(&buf, resp); err != nil {
		t.Fatalf("Failed to write error response: %v", err)
	}

	readResp, err := protocol.ReceiveResponse(&buf)
	if err != nil {
		t.Fatalf("Erro ao ler error response: %v", err)
	}

	if readResp.Error != resp.Error {
		t.Errorf("Error: expected %s, got %s", resp.Error, readResp.Error)
	}
}

func TestProtocolPayloadTooLarge(t *testing.T) {
	protocol := NewProtocol()

	largePayload := make([]byte, MaxPayloadSize+1)
	
	var buf bytes.Buffer
	err := protocol.WriteFrame(&buf, largePayload)
	
	if err == nil {
		t.Error("Should return error for payload too large")
	}
}

func TestNextRequestID(t *testing.T) {
	id1 := NextRequestID()
	id2 := NextRequestID()
	id3 := NextRequestID()

	if id2 != id1+1 {
		t.Errorf("IDs devem ser sequenciais: %d, %d", id1, id2)
	}
	if id3 != id2+1 {
		t.Errorf("IDs devem ser sequenciais: %d, %d", id2, id3)
	}
}

func BenchmarkProtocolWriteRead(b *testing.B) {
	protocol := NewProtocol()
	req := &Request{
		ID:     1,
		Method: "POST",
		Path:   "/api/users",
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
		},
		Body: []byte(`{"name":"John"}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		protocol.SendRequest(&buf, req)
		protocol.ReceiveRequest(&buf)
	}
}

func BenchmarkMsgPackVsJSON(b *testing.B) {
	protocol := NewProtocol()
	req := &Request{
		ID:     1,
		Method: "POST",
		Path:   "/api/users",
		Headers: map[string][]string{
			"Content-Type":    {"application/json"},
			"Accept":          {"application/json"},
			"X-Request-ID":    {"abc-123"},
			"Authorization":   {"Bearer token123"},
			"Accept-Language": {"en-US,en;q=0.9"},
		},
		Body:       []byte(`{"name":"John Doe","email":"john@example.com","age":30,"active":true}`),
		RemoteAddr: "192.168.1.100:54321",
		Host:       "api.example.com",
		Scheme:     "https",
		TimeoutMs:  30000,
	}

	b.Run("MsgPack", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var buf bytes.Buffer
			protocol.SendRequest(&buf, req)
			protocol.ReceiveRequest(&buf)
		}
	})
}
