package protocol

import (
	"net/http"
	"strings"
)

// HopByHopHeaders are HTTP/1.1 hop-by-hop headers that must not be
// forwarded by proxies. These are defined in RFC 9110 § 5.4.
var HopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// IsHopByHop reports whether name is a hop-by-hop header that must not be
// forwarded. Includes the static RFC 9110 list AND any header named in the
// request's Connection header (per RFC 9110 § 7.6.1) — without the second
// half a peer can smuggle headers across the proxy by listing them in
// Connection.
func IsHopByHop(name string, connectionHeader []string) bool {
	canon := http.CanonicalHeaderKey(name)
	if HopByHopHeaders[canon] {
		return true
	}
	for _, list := range connectionHeader {
		for _, tok := range strings.Split(list, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), canon) {
				return true
			}
		}
	}
	return false
}
