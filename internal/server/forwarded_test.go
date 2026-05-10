package server

import (
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestSanitizeAndFillForwardedDefault(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "relay.example.com"
	r.RemoteAddr = "203.0.113.7:54321"
	headers := map[string][]string{
		"X-Forwarded-For":   {"1.2.3.4"},
		"X-Forwarded-Proto": {"https"},
		"X-Real-Ip":         {"9.9.9.9"},
		"Forwarded":         {"for=10.10.10.10"},
		"Host":              {"relay.example.com"},
	}
	sanitizeAndFillForwarded(headers, r)

	if got := headers["X-Forwarded-For"]; !reflect.DeepEqual(got, []string{"203.0.113.7"}) {
		t.Errorf("X-Forwarded-For = %v, want server-observed IP", got)
	}
	if got := headers["X-Real-Ip"]; !reflect.DeepEqual(got, []string{"203.0.113.7"}) {
		t.Errorf("X-Real-Ip = %v", got)
	}
	if got := headers["X-Forwarded-Proto"]; !reflect.DeepEqual(got, []string{"http"}) {
		t.Errorf("X-Forwarded-Proto = %v, want http (no TLS, no trust)", got)
	}
	if got := headers["X-Forwarded-Host"]; !reflect.DeepEqual(got, []string{"relay.example.com"}) {
		t.Errorf("X-Forwarded-Host = %v", got)
	}
	if _, ok := headers["Forwarded"]; ok {
		t.Errorf("Forwarded header must be stripped")
	}
}

func TestSanitizeAndFillForwardedTrustProxy(t *testing.T) {
	prev := trustProxyHeaders
	trustProxyHeaders = true
	defer func() { trustProxyHeaders = prev }()

	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "relay.example.com"
	r.RemoteAddr = "203.0.113.7:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	r.Header.Set("X-Forwarded-Proto", "https")
	headers := map[string][]string{
		"X-Forwarded-For":   {"1.2.3.4"},
		"X-Forwarded-Proto": {"https"},
	}
	sanitizeAndFillForwarded(headers, r)

	if got := headers["X-Forwarded-For"]; !reflect.DeepEqual(got, []string{"1.2.3.4"}) {
		t.Errorf("trust=true must honour upstream X-Forwarded-For, got %v", got)
	}
	if got := headers["X-Forwarded-Proto"]; !reflect.DeepEqual(got, []string{"https"}) {
		t.Errorf("trust=true must honour upstream X-Forwarded-Proto, got %v", got)
	}
}

func TestStripReservedSetCookies(t *testing.T) {
	in := map[string][]string{
		"Set-Cookie": {
			"sessionid=abc; Path=/",
			"__tunnel_dashboard=stolen; Path=/",
			"theme=dark",
			"__TUNNEL_ID=evil; Path=/",
		},
		"Content-Type": {"text/html"},
	}
	out := stripReservedSetCookies(in)
	cookies := out["Set-Cookie"]
	if len(cookies) != 2 {
		t.Fatalf("expected 2 cookies after strip, got %d: %v", len(cookies), cookies)
	}
	for _, c := range cookies {
		if c == "__tunnel_dashboard=stolen; Path=/" || c == "__TUNNEL_ID=evil; Path=/" {
			t.Errorf("reserved cookie not stripped: %q", c)
		}
	}
	if out["Content-Type"][0] != "text/html" {
		t.Error("non-Set-Cookie header lost")
	}
}
