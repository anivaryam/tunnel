package protocol_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/coder/websocket"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Logf("accept: %v", err)
			return
		}
		defer conn.CloseNow()

		env, body, err := protocol.ReadEnvelope(r.Context(), conn)
		if err != nil {
			t.Logf("read envelope: %v", err)
			return
		}

		cw := protocol.NewConnWriter(conn)
		if err := cw.WriteEnvelope(r.Context(), env, body); err != nil {
			t.Logf("write envelope: %v", err)
			return
		}

		conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer srv.Close()

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	cw := protocol.NewConnWriter(conn)
	sent := protocol.Envelope{
		Type:      protocol.TypeHTTPRequest,
		RequestID: "req-123",
		HTTPRequest: &protocol.HTTPRequestPayload{
			Method: "POST",
			Path:   "/api/users",
			Query:  "page=1",
			Headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			ContentLength: 13,
		},
	}
	body := []byte(`{"name":"hi"}`)

	if err := cw.WriteEnvelope(ctx, sent, body); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, gotBody, err := protocol.ReadEnvelope(ctx, conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if got.Type != sent.Type {
		t.Errorf("type: got %q, want %q", got.Type, sent.Type)
	}
	if got.RequestID != sent.RequestID {
		t.Errorf("requestID: got %q, want %q", got.RequestID, sent.RequestID)
	}
	if got.HTTPRequest == nil {
		t.Fatal("HTTPRequest is nil")
	}
	if got.HTTPRequest.Method != "POST" {
		t.Errorf("method: got %q, want POST", got.HTTPRequest.Method)
	}
	if got.HTTPRequest.Path != "/api/users" {
		t.Errorf("path: got %q, want /api/users", got.HTTPRequest.Path)
	}
	if string(gotBody) != `{"name":"hi"}` {
		t.Errorf("body: got %q, want %q", gotBody, body)
	}
}

func TestEnvelopeOnlyRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		env, err := protocol.ReadEnvelopeOnly(r.Context(), conn)
		if err != nil {
			return
		}
		cw := protocol.NewConnWriter(conn)
		if err := cw.WriteEnvelopeOnly(r.Context(), env); err != nil {
			return
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer srv.Close()

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	cw := protocol.NewConnWriter(conn)
	sent := protocol.Envelope{
		Type: protocol.TypeAssignment,
		Assignment: &protocol.TunnelAssignment{
			TunnelID:  "abc12345",
			PublicURL: "https://relay.example.com/t/abc12345",
		},
	}
	if err := cw.WriteEnvelopeOnly(ctx, sent); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := protocol.ReadEnvelopeOnly(ctx, conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Assignment == nil {
		t.Fatal("assignment is nil")
	}
	if got.Assignment.TunnelID != "abc12345" {
		t.Errorf("tunnel id: got %q, want abc12345", got.Assignment.TunnelID)
	}
}

func TestEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		env, body, err := protocol.ReadEnvelope(r.Context(), conn)
		if err != nil {
			return
		}
		cw := protocol.NewConnWriter(conn)
		if err := cw.WriteEnvelope(r.Context(), env, body); err != nil {
			return
		}
		conn.Close(websocket.StatusNormalClosure, "done")
	}))
	defer srv.Close()

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	cw := protocol.NewConnWriter(conn)
	sent := protocol.Envelope{
		Type:      protocol.TypeHTTPRequest,
		RequestID: "req-456",
		HTTPRequest: &protocol.HTTPRequestPayload{
			Method:        "GET",
			Path:          "/health",
			ContentLength: 0,
		},
	}
	if err := cw.WriteEnvelope(ctx, sent, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, gotBody, err := protocol.ReadEnvelope(ctx, conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.HTTPRequest.Method != "GET" {
		t.Errorf("method: got %q, want GET", got.HTTPRequest.Method)
	}
	if len(gotBody) != 0 {
		t.Errorf("body should be empty, got %d bytes", len(gotBody))
	}
}
