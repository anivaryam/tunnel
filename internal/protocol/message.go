package protocol

// Envelope is the metadata frame sent as JSON over a WebSocket text message.
// The body (if any) follows as a separate binary frame.
type Envelope struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`

	// Only one of these is set, depending on Type.
	HTTPRequest  *HTTPRequestPayload  `json:"http_request,omitempty"`
	HTTPResponse *HTTPResponsePayload `json:"http_response,omitempty"`
	Assignment   *TunnelAssignment    `json:"assignment,omitempty"`

	// TCP stream messages.
	StreamOpen    *StreamOpenPayload    `json:"stream_open,omitempty"`
	StreamOpenAck *StreamOpenAckPayload `json:"stream_open_ack,omitempty"`
	StreamData    *StreamDataPayload    `json:"stream_data,omitempty"`
	StreamClose   *StreamClosePayload   `json:"stream_close,omitempty"`

	// WebSocket proxy messages.
	WSOpen    *WSOpenPayload    `json:"ws_open,omitempty"`
	WSOpenAck *WSOpenAckPayload `json:"ws_open_ack,omitempty"`
	WSFrame   *WSFramePayload   `json:"ws_frame,omitempty"`
	WSClose   *WSClosePayload   `json:"ws_close,omitempty"`

	// HTTP response streaming messages (SSE / chunked downloads).
	// Flow: client sends HTTPResponse{Streaming:true} → chunks → HTTPStreamEnd.
	// Server sends HTTPStreamCancel to abort local HTTP work when the browser
	// disconnects or the relay stops waiting for a response.
	HTTPStreamChunk  *HTTPStreamChunkPayload  `json:"http_stream_chunk,omitempty"`
	HTTPStreamEnd    *HTTPStreamEndPayload    `json:"http_stream_end,omitempty"`
	HTTPStreamCancel *HTTPStreamCancelPayload `json:"http_stream_cancel,omitempty"`

	// HTTP request streaming messages (large uploads).
	// Flow: server sends HTTPRequest{Streaming:true} (no body) → chunks → HTTPRequestEnd.
	HTTPRequestChunk *HTTPRequestChunkPayload `json:"http_request_chunk,omitempty"`
	HTTPRequestEnd   *HTTPRequestEndPayload   `json:"http_request_end,omitempty"`
}

// Message types.
const (
	TypeHTTPRequest  = "http_request"
	TypeHTTPResponse = "http_response"
	TypeAssignment   = "assignment"

	// TCP stream message types.
	TypeStreamOpen    = "stream_open"
	TypeStreamOpenAck = "stream_open_ack"
	TypeStreamData    = "stream_data"
	TypeStreamClose   = "stream_close"

	// WebSocket proxy message types.
	TypeWSOpen    = "ws_open"
	TypeWSOpenAck = "ws_open_ack"
	TypeWSFrame   = "ws_frame"
	TypeWSClose   = "ws_close"

	// HTTP response streaming message types (SSE / chunked downloads).
	TypeHTTPStreamChunk  = "http_stream_chunk"
	TypeHTTPStreamEnd    = "http_stream_end"
	TypeHTTPStreamCancel = "http_stream_cancel"

	// HTTP request streaming message types (large uploads).
	// Flow: server sends HTTPRequest{Streaming:true} → chunks → HTTPRequestEnd.
	TypeHTTPRequestChunk = "http_request_chunk"
	TypeHTTPRequestEnd   = "http_request_end"
)

// HTTPRequestPayload describes an incoming HTTP request to be forwarded through the tunnel.
// When Streaming is true the body is omitted from the initial envelope; the server sends it
// via TypeHTTPRequestChunk messages followed by TypeHTTPRequestEnd.
type HTTPRequestPayload struct {
	Method        string              `json:"method"`
	Path          string              `json:"path"`
	Query         string              `json:"query,omitempty"`
	Headers       map[string][]string `json:"headers,omitempty"`
	ContentLength int64               `json:"content_length"`
	Streaming     bool                `json:"streaming,omitempty"`
}

// HTTPResponsePayload describes the HTTP response returned by the local server.
// When Streaming is true the body is omitted; the client sends it via
// TypeHTTPStreamChunk messages followed by TypeHTTPStreamEnd.
type HTTPResponsePayload struct {
	StatusCode    int                 `json:"status_code"`
	Headers       map[string][]string `json:"headers,omitempty"`
	ContentLength int64               `json:"content_length"`
	Streaming     bool                `json:"streaming,omitempty"`
}

// HTTPStreamChunkPayload carries one chunk of a streaming HTTP response body.
// The actual bytes follow as the binary frame.
type HTTPStreamChunkPayload struct {
	RequestID string `json:"request_id"`
}

// HTTPStreamEndPayload signals the end of a streaming HTTP response.
type HTTPStreamEndPayload struct {
	RequestID string `json:"request_id"`
}

// HTTPStreamCancelPayload is sent server→client so the client can cancel the
// local HTTP request for a closed stream or timed-out relay request.
type HTTPStreamCancelPayload struct {
	RequestID string `json:"request_id"`
}

// TunnelAssignment is sent from server to client after a successful WebSocket handshake.
type TunnelAssignment struct {
	TunnelID  string `json:"tunnel_id"`
	PublicURL string `json:"public_url"`
	Mode      string `json:"mode,omitempty"`     // "http", "tcp", or "udp"
	TCPAddr   string `json:"tcp_addr,omitempty"` // public TCP address for TCP tunnels
	UDPAddr   string `json:"udp_addr,omitempty"` // public UDP address for UDP tunnels
}

// StreamOpenPayload is sent from server to client when a new TCP connection arrives.
type StreamOpenPayload struct {
	StreamID   string `json:"stream_id"`
	RemoteAddr string `json:"remote_addr"`
}

// StreamOpenAckPayload is sent from client to server confirming local TCP connection.
type StreamOpenAckPayload struct {
	StreamID string `json:"stream_id"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// StreamDataPayload carries TCP data in either direction. The actual bytes are in the binary frame.
type StreamDataPayload struct {
	StreamID string `json:"stream_id"`
}

// StreamClosePayload signals the end of a TCP stream.
type StreamClosePayload struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// WSOpenPayload is sent from server to client to open a WebSocket to the local server.
type WSOpenPayload struct {
	StreamID string              `json:"stream_id"`
	Path     string              `json:"path"`
	Query    string              `json:"query,omitempty"`
	Headers  map[string][]string `json:"headers,omitempty"`
}

// WSOpenAckPayload is sent from client to server confirming local WebSocket connection.
type WSOpenAckPayload struct {
	StreamID string `json:"stream_id"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// WSFramePayload carries a WebSocket frame in either direction. The actual bytes are in the binary frame.
type WSFramePayload struct {
	StreamID    string `json:"stream_id"`
	MessageType int    `json:"message_type"` // 1=text, 2=binary
}

// WSClosePayload signals the end of a proxied WebSocket connection.
type WSClosePayload struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// HTTPRequestChunkPayload carries one chunk of a streaming HTTP request body.
// The actual bytes follow as the binary frame.
type HTTPRequestChunkPayload struct {
	RequestID string `json:"request_id"`
}

// HTTPRequestEndPayload signals the end of a streaming HTTP request body.
type HTTPRequestEndPayload struct {
	RequestID string `json:"request_id"`
}

// MaxBodySize is the maximum allowed body size for buffered (non-streaming) requests (10 MB).
const MaxBodySize = 10 * 1024 * 1024

// MaxStreamChunk is the maximum size of a single TCP stream data frame (64 KB).
const MaxStreamChunk = 64 * 1024

// RequestStreamThreshold is the request body size above which the server streams
// the body to the client instead of buffering it. Requests larger than this or
// with unknown Content-Length use TypeHTTPRequestChunk messages.
const RequestStreamThreshold = 1 * 1024 * 1024 // 1 MB
