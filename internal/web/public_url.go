package web

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Absolute links handed back to clients (hosted image URLs, the MCP SSE
// endpoint) must be reachable from wherever the caller actually is. Behind a
// reverse proxy the inbound Host header is whatever the proxy chose to send —
// it may be the upstream address (127.0.0.1:4141), a placeholder server_name,
// or a public hostname stripped of its non-standard port. Deriving links from
// r.Host alone therefore produces URLs the client cannot open.
//
// publicBaseURL resolves "scheme://host[:port][/prefix]" using, in order:
//
//  1. The operator override (admin setting, or M365_PUBLIC_BASE_URL). This is
//     the only source that stays correct when the proxy rewrites or omits the
//     forwarding headers, so it wins outright.
//  2. X-Forwarded-Host / -Proto / -Port from the reverse proxy.
//  3. The request's own Host header (correct for direct LAN access).
func (s *Server) publicBaseURL(r *http.Request) string {
	if base := normalizePublicBase(s.publicBaseOverride()); base != "" {
		return base
	}
	if r == nil {
		return ""
	}
	scheme := forwardedScheme(r)
	host := forwardedHost(r, scheme)
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// publicBaseOverride reads the configured public base URL. The runtime setting
// is seeded from M365_PUBLIC_BASE_URL, but the env var is also consulted
// directly so the override still applies before settings are wired up.
func (s *Server) publicBaseOverride() string {
	if s != nil && s.settings != nil {
		if v := strings.TrimSpace(s.settings.get().PublicBaseURL); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv("M365_PUBLIC_BASE_URL"))
}

// normalizePublicBase accepts the forms operators actually type — with or
// without a scheme, with or without a trailing slash or path prefix — and
// reduces them to a base that can be concatenated with an absolute path.
// Returns "" when the input cannot be parsed into an http(s) authority.
func normalizePublicBase(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	// "365api.example.net:52325" parses as scheme "365api.example.net" with an
	// opaque body, so assume https:// when no scheme was supplied.
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	if !validForwardedHost(u.Host) {
		return ""
	}
	base := scheme + "://" + u.Host
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		base += p
	}
	return base
}

func forwardedScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if v := firstForwardedValue(r.Header.Get("X-Forwarded-Proto")); v != "" {
		switch strings.ToLower(v) {
		case "http":
			return "http"
		case "https":
			return "https"
		}
	}
	if v := firstForwardedValue(r.Header.Get("X-Forwarded-Scheme")); v != "" {
		switch strings.ToLower(v) {
		case "http":
			return "http"
		case "https":
			return "https"
		}
	}
	return "http"
}

// forwardedHost prefers X-Forwarded-Host, re-attaching X-Forwarded-Port when
// the proxy forwarded the hostname without the non-default public port — the
// common case that turns https://host:52325/... into an unreachable
// https://host/... link.
func forwardedHost(r *http.Request, scheme string) string {
	host := firstForwardedValue(r.Header.Get("X-Forwarded-Host"))
	if host != "" && validForwardedHost(host) {
		if !hasPort(host) {
			if port := firstForwardedValue(r.Header.Get("X-Forwarded-Port")); isPlainPort(port) && !isDefaultPort(scheme, port) {
				host += ":" + port
			}
		}
		return host
	}
	if validForwardedHost(r.Host) {
		return r.Host
	}
	return ""
}

func firstForwardedValue(raw string) string {
	return strings.TrimSpace(strings.SplitN(raw, ",", 2)[0])
}

// validForwardedHost rejects values that would break out of the authority
// component of the generated URL. These headers are client-supplied, and the
// result is embedded in markdown links, so a host containing a slash, quote or
// whitespace must never be interpolated verbatim.
func validForwardedHost(h string) bool {
	if h == "" || len(h) > 255 {
		return false
	}
	if strings.ContainsAny(h, "/\\?#@ \t\r\n\"'<>") {
		return false
	}
	// Never publish documentation/test placeholders as an image URL. They
	// commonly leak from an NPM default proxy host or an old example setting;
	// returning a relative URL is safer than handing the client a dead link.
	if u, err := url.Parse("//" + h); err == nil {
		switch strings.ToLower(u.Hostname()) {
		case "example.com", "example.net", "example.org", "example.invalid",
			"placeholder.invalid", "unregistered.example":
			return false
		}
	}
	return true
}

// hasPort reports whether host already carries a :port suffix, tolerating
// bracketed IPv6 literals ("[::1]" has no port, "[::1]:8080" does).
func hasPort(host string) bool {
	if strings.HasPrefix(host, "[") {
		return strings.Contains(host[strings.LastIndex(host, "]")+1:], ":")
	}
	return strings.Contains(host, ":")
}

func isPlainPort(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isDefaultPort(scheme, port string) bool {
	return (scheme == "https" && port == "443") || (scheme == "http" && port == "80")
}
