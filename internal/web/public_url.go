package web

import (
	"fmt"
	"net"
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
	for _, raw := range []string{
		r.Header.Get("X-Forwarded-Proto"),
		r.Header.Get("X-Forwarded-Scheme"),
		forwardedHeaderField(r, "proto"),
	} {
		switch strings.ToLower(firstForwardedValue(raw)) {
		case "http":
			return "http"
		case "https":
			return "https"
		}
	}
	return "http"
}

// forwardedHeaderField reads one parameter of the RFC 7239 Forwarded header,
// which carries the same information as the X-Forwarded-* trio in a single
// field: `Forwarded: for=1.2.3.4;host=example.net:52325;proto=https`.
//
// Elements are scanned left to right and the first one carrying the field wins,
// which is the outermost proxy's view. Elements are appended as a request passes
// through proxies, so a nearer one may not repeat every field.
func forwardedHeaderField(r *http.Request, field string) string {
	raw := strings.TrimSpace(r.Header.Get("Forwarded"))
	if raw == "" {
		return ""
	}
	for _, element := range strings.Split(raw, ",") {
		for _, part := range strings.Split(element, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), field) {
				continue
			}
			// RFC 7239 allows the value to be quoted.
			if v = strings.Trim(strings.TrimSpace(v), "\""); v != "" {
				return v
			}
		}
	}
	return ""
}

// forwardedHost prefers X-Forwarded-Host as the hostname, re-attaching the
// public port when the proxy forwarded the host without one — the case that
// turns https://host:52325/... into an unreachable https://host/... link.
func forwardedHost(r *http.Request, scheme string) string {
	candidates := []string{
		firstForwardedValue(r.Header.Get("X-Forwarded-Host")),
		forwardedHeaderField(r, "host"),
	}
	// A candidate that already states the port is the proxy's own answer, so it
	// is preferred outright: the others may name a different address.
	for _, h := range candidates {
		if h != "" && validForwardedHost(h) && hasPort(h) {
			return h
		}
	}
	for _, h := range candidates {
		if h != "" && validForwardedHost(h) {
			return h + portSuffixFor(r, scheme)
		}
	}
	if validForwardedHost(r.Host) {
		return r.Host
	}
	return ""
}

// publicBaseHasPort reports whether a resolved base URL states a port
// explicitly. A base without one is legitimate (the proxy really is on the
// scheme's default port) but it is also what a dropped port looks like, and the
// two are indistinguishable from here - which is why it is worth saying so in
// the log rather than letting the operator work it out from a dead link.
func publicBaseHasPort(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Port() != ""
}

// publicBaseDiagnostics renders the inputs a base URL was derived from, for the
// log line that explains a link the operator cannot open.
func (s *Server) publicBaseDiagnostics(r *http.Request) string {
	if r == nil {
		return "no request"
	}
	return fmt.Sprintf("source=%s host=%q x-forwarded-host=%q x-forwarded-port=%q x-forwarded-proto=%q forwarded=%q override=%q",
		s.publicBaseSource(r), r.Host,
		r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-Port"),
		r.Header.Get("X-Forwarded-Proto"), r.Header.Get("Forwarded"),
		strings.TrimSpace(os.Getenv("M365_PUBLIC_BASE_URL")))
}

// publicBaseSource names the input the resolved base URL came from. The
// derivation is otherwise invisible: a proxy that drops the public port produces
// a link that points nowhere, and nothing in the response or the console says
// which header the gateway actually used, so the only way to tell was to guess.
func (s *Server) publicBaseSource(r *http.Request) string {
	if base := normalizePublicBase(s.publicBaseOverride()); base != "" {
		return "override"
	}
	if r == nil {
		return "none"
	}
	if h := firstForwardedValue(r.Header.Get("X-Forwarded-Host")); h != "" && validForwardedHost(h) {
		return "x-forwarded-host"
	}
	if h := forwardedHeaderField(r, "host"); h != "" && validForwardedHost(h) {
		return "forwarded"
	}
	if validForwardedHost(r.Host) {
		return "request-host"
	}
	return "none"
}

// portSuffixFor returns ":port" to append to a forwarded hostname that arrived
// without one, or "" when no port should be appended.
//
// X-Forwarded-Port is the proxy's explicit answer and wins. When the proxy does
// not send it, the request's own Host header usually still carries the port the
// client actually connected to, and dropping it hands the caller a link on the
// default port that nothing is listening on. A plain nginx pair is enough to
// produce that:
//
//	proxy_set_header Host $http_host;           # host:52325, the public port
//	proxy_set_header X-Forwarded-Host $host;    # host only, no port
//
// Only the port is borrowed from there. The hostname from X-Forwarded-Host still
// wins, because the Host header may name an upstream address rather than the one
// the caller can reach.
func portSuffixFor(r *http.Request, scheme string) string {
	if port := firstForwardedValue(r.Header.Get("X-Forwarded-Port")); isPlainPort(port) {
		if isDefaultPort(scheme, port) {
			return ""
		}
		return ":" + port
	}
	host := firstForwardedValue(r.Host)
	if !validForwardedHost(host) {
		return ""
	}
	_, port, err := net.SplitHostPort(host)
	if err != nil || !isPlainPort(port) || isDefaultPort(scheme, port) {
		return ""
	}
	return ":" + port
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
