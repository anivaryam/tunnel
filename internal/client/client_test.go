package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anivaryam/tunnel/internal/protocol"
	"github.com/coder/websocket"
)

// TestResetConnStateClearsMaps verifies that the per-connection teardown drains
// all stream-bookkeeping maps and unwinds blocked consumers, so reconnects don't
// accumulate stale entries (the leak fixed alongside this test).
func TestResetConnStateClearsMaps(t *testing.T) {
	c := NewClient("http://example", "", 8080)

	// Populate each map as the read loop would during an active connection.
	requestCancelled := false
	c.requestCancels["req-http"] = func() { requestCancelled = true }

	pr, pw := io.Pipe()
	c.reqPipes["req-up"] = pw
	// A reader blocked on the pipe should unblock with an error after reset.
	readErr := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(pr)
		readErr <- err
	}()

	c.resetConnState()

	if len(c.requestCancels) != 0 {
		t.Errorf("requestCancels not cleared: %d entries", len(c.requestCancels))
	}
	if len(c.reqPipes) != 0 {
		t.Errorf("reqPipes not cleared: %d entries", len(c.reqPipes))
	}
	if len(c.wsConns) != 0 {
		t.Errorf("wsConns not cleared: %d entries", len(c.wsConns))
	}
	if !requestCancelled {
		t.Error("request cancel func was not invoked")
	}
	if err := <-readErr; err == nil {
		t.Error("pipe reader did not receive the disconnect error")
	}
}

func TestCancelRequestCancelsInFlightHTTP(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer local.Close()

	var port int
	fmt.Sscanf(strings.TrimPrefix(local.URL, "http://127.0.0.1:"), "%d", &port)

	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			if _, _, err := protocol.ReadEnvelope(r.Context(), conn); err != nil {
				return
			}
		}
	}))
	defer wsSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(wsSrv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	c := NewClient("http://example", "", port)
	c.writer = protocol.NewConnWriter(conn)
	env := protocol.Envelope{
		Type:      protocol.TypeHTTPRequest,
		RequestID: "req-cancel",
		HTTPRequest: &protocol.HTTPRequestPayload{
			Method: http.MethodGet,
			Path:   "/",
		},
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.handleRequest(ctx, env, nil)
	}()

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	c.cancelRequest("req-cancel")

	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("local request was not cancelled")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("handleRequest did not return after cancel")
	}
}
