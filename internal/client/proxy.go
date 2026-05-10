package client

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/anivaryam/tunnel/internal/protocol"
)

// ForwardRequest forwards an HTTP request envelope to the local server and returns the response.
func ForwardRequest(localPort int, env protocol.Envelope, body []byte) (protocol.Envelope, []byte, error) {
	req := env.HTTPRequest
	if req == nil {
		return protocol.Envelope{}, nil, fmt.Errorf("nil http request payload")
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", localPort, req.Path)
	if req.Query != "" {
		url += "?" + req.Query
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	httpReq, err := http.NewRequest(req.Method, url, bodyReader)
	if err != nil {
		return protocol.Envelope{}, nil, fmt.Errorf("build request: %w", err)
	}

	// Copy headers, filtering hop-by-hop (including Connection-listed tokens).
	connHeader := req.Headers["Connection"]
	for k, vals := range req.Headers {
		if protocol.IsHopByHop(k, connHeader) {
			continue
		}
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return protocol.Envelope{}, nil, fmt.Errorf("local request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := protocol.ReadBody(resp.Body)
	if err != nil {
		return protocol.Envelope{}, nil, fmt.Errorf("read response body: %w", err)
	}

	// Filter response hop-by-hop headers (including Connection-listed tokens).
	respConnHeader := resp.Header["Connection"]
	respHeaders := make(map[string][]string, len(resp.Header))
	for k, vals := range resp.Header {
		if protocol.IsHopByHop(k, respConnHeader) {
			continue
		}
		respHeaders[k] = vals
	}

	respEnv := protocol.Envelope{
		Type:      protocol.TypeHTTPResponse,
		RequestID: env.RequestID,
		HTTPResponse: &protocol.HTTPResponsePayload{
			StatusCode:    resp.StatusCode,
			Headers:       respHeaders,
			ContentLength: int64(len(respBody)),
		},
	}

	return respEnv, respBody, nil
}
