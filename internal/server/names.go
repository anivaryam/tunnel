package server

import "regexp"

// nameRegex enforces DNS-label-safe names: 1-32 chars, lowercase alphanumeric
// or hyphen, no leading/trailing hyphen.
var nameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// reservedNames cannot be claimed because they collide with server control
// paths, are commonly used for phishing under shared TLS, or are conventional
// infrastructure subdomains.
var reservedNames = map[string]struct{}{
	// server control paths
	"dashboard": {},
	"metrics":   {},
	"health":    {},
	"ws":        {},
	"t":         {},
	"api":       {},
	"admin":     {},
	// phishing / brand-impersonation surface
	"www":      {},
	"mail":     {},
	"email":    {},
	"login":    {},
	"signin":   {},
	"signup":   {},
	"register": {},
	"auth":     {},
	"account":  {},
	"accounts": {},
	"billing":  {},
	"pay":      {},
	"payment":  {},
	"secure":   {},
	"support":  {},
	"help":     {},
	// common infrastructure subdomains
	"app":     {},
	"apps":    {},
	"static":  {},
	"cdn":     {},
	"assets":  {},
	"media":   {},
	"img":     {},
	"images":  {},
	"files":   {},
	"docs":    {},
	"blog":    {},
	"status":  {},
	"www2":    {},
	"web":     {},
	"root":    {},
	"system":  {},
	"public":  {},
	"private": {},
	"test":    {},
	"staging": {},
	"dev":     {},
	"prod":    {},
}

// ValidName reports whether s is a syntactically valid, non-reserved tunnel name.
func ValidName(s string) bool {
	if _, reserved := reservedNames[s]; reserved {
		return false
	}
	return nameRegex.MatchString(s)
}
