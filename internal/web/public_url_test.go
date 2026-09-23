package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNormalizePublicBase(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"host and port without scheme", "365api.example.net:52325", "https://365api.example.net:52325"},
		{"explicit https with port", "https://365api.example.net:52325", "https://365api.example.net:52325"},
		{"explicit http", "http://10.0.0.3:4141", "http://10.0.0.3:4141"},
		{"trailing slash dropped", "https://api.example.net:52325/", "https://api.example.net:52325"},
		{"sub-path mount preserved", "https://api.example.net/m365/", "https://api.example.net/m365"},
		{"bare hostname", "api.example.net", "https://api.example.net"},
		{"unsupported scheme rejected", "ftp://api.example.net", ""},
		{"whitespace injection rejected", "https://api.example.net foo", ""},
		{"path traversal in host rejected", "https://api.example.net\\evil", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePublicBase(tc.in); got != tc.want {
				t.Fatalf("normalizePublicBase(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Direct LAN access must keep working exactly as before: no override, no
// forwarding headers, so the link is derived from the request's own Host.
func TestPublicBaseURLFallsBackToRequestHost(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "10.0.0.3:4141"
	if got := (&Server{}).publicBaseURL(r); got != "http://10.0.0.3:4141" {
		t.Fatalf("got %q, want http://10.0.0.3:4141", got)
	}
}

// A reverse proxy that forwards the public host and scheme must win over the
// upstream Host it dialled (127.0.0.1:4141), otherwise the client is handed an
// address that only resolves inside the LAN.
func TestPublicBaseURLUsesForwardedHost(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "127.0.0.1:4141"
	r.Header.Set("X-Forwarded-Host", "365api.example.net:52325")
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := (&Server{}).publicBaseURL(r); got != "https://365api.example.net:52325" {
		t.Fatalf("got %q, want https://365api.example.net:52325", got)
	}
}

// The reported failure mode: the proxy forwards the hostname but drops the
// non-standard public port, so the port must be recovered from
// X-Forwarded-Port or the link is unreachable.
func TestPublicBaseURLReattachesForwardedPort(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "127.0.0.1:4141"
	r.Header.Set("X-Forwarded-Host", "365api.example.net")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Port", "52325")
	if got := (&Server{}).publicBaseURL(r); got != "https://365api.example.net:52325" {
		t.Fatalf("got %q, want https://365api.example.net:52325", got)
	}
}

// The port has to survive when the proxy forwards the hostname without it and
// sends no X-Forwarded-Port, which is what a plain nginx pair produces:
//
//	proxy_set_header Host $http_host;         # host:52325, the public port
//	proxy_set_header X-Forwarded-Host $host;  # host only
//
// The hostname from X-Forwarded-Host still wins, but dropping the port hands the
// client a link on the default port that nothing is listening on.
func TestPublicBaseURLBorrowsPortFromRequestHost(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "365api.example.net:52325"
	r.Header.Set("X-Forwarded-Host", "365api.example.net")
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := (&Server{}).publicBaseURL(r); got != "https://365api.example.net:52325" {
		t.Fatalf("got %q, want https://365api.example.net:52325", got)
	}
}

// Explicit beats inferred: X-Forwarded-Port is the proxy telling us the public
// port, so it must not be overridden by whatever the Host header happens to be.
func TestPublicBaseURLPrefersForwardedPortOverHostPort(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "127.0.0.1:4141"
	r.Header.Set("X-Forwarded-Host", "365api.example.net")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Port", "52325")
	if got := (&Server{}).publicBaseURL(r); got != "https://365api.example.net:52325" {
		t.Fatalf("got %q, want https://365api.example.net:52325", got)
	}
}

// A host that already carries a port is the proxy's own answer; appending the
// Host port on top of it would produce host:52325:4141.
func TestPublicBaseURLDoesNotDoubleAppendPort(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "127.0.0.1:4141"
	r.Header.Set("X-Forwarded-Host", "365api.example.net:52325")
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := (&Server{}).publicBaseURL(r); got != "https://365api.example.net:52325" {
		t.Fatalf("got %q, want https://365api.example.net:52325", got)
	}
}

// Nothing to borrow when the request Host carries no port either, and a default
// port is still left off.
func TestPublicBaseURLPortBorrowEdgeCases(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	cases := []struct {
		name   string
		host   string
		scheme string
		want   string
	}{
		{"no port anywhere", "365api.example.net", "https", "https://365api.example.net"},
		{"default port not appended", "365api.example.net:443", "https", "https://365api.example.net"},
		{"default port on http", "365api.example.net:80", "http", "http://365api.example.net"},
		{"ipv6 host with port", "[2001:db8::1]:52325", "https", "https://365api.example.net:52325"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/images/generations", nil)
			r.Host = tc.host
			r.Header.Set("X-Forwarded-Host", "365api.example.net")
			r.Header.Set("X-Forwarded-Proto", tc.scheme)
			if got := (&Server{}).publicBaseURL(r); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The port reaching the client matters on the concrete output, not only in
// publicBaseURL: this is the link a client is handed for a generated image.
func TestGeneratedImageURLCarriesTheBorrowedPort(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "365api.example.net:52325"
	r.Header.Set("X-Forwarded-Host", "365api.example.net")
	r.Header.Set("X-Forwarded-Proto", "https")
	got := (&Server{}).generatedImageURL(r, "abc.png")
	want := "https://365api.example.net:52325/v1/images/files/abc.png"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// RFC 7239 Forwarded carries the same information as the X-Forwarded-* trio in
// one field. A proxy that sends only this one would otherwise leave the public
// port unknown, which is the failure this file exists for.
func TestPublicBaseURLReadsRFC7239Forwarded(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	cases := []struct {
		name      string
		host      string
		forwarded string
		want      string
	}{
		{"host with port", "127.0.0.1:4141",
			`for=1.2.3.4;host=365api.example.net:52325;proto=https`, "https://365api.example.net:52325"},
		{"quoted value", "127.0.0.1:4141",
			`for="1.2.3.4";host="365api.example.net:52325";proto="https"`, "https://365api.example.net:52325"},
		{"host without port borrows from Host", "365api.example.net:52325",
			`host=365api.example.net;proto=https`, "https://365api.example.net:52325"},
		{"host without port and nothing to borrow", "127.0.0.1",
			`host=365api.example.net;proto=https`, "https://365api.example.net"},
		{"http proto", "127.0.0.1:8080",
			`host=365api.example.net:8080;proto=http`, "http://365api.example.net:8080"},
		{"first element without the field", "365api.example.net:52325",
			`for=1.2.3.4, host=365api.example.net;proto=https`, "https://365api.example.net:52325"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/images/generations", nil)
			r.Host = tc.host
			r.Header.Set("Forwarded", tc.forwarded)
			if got := (&Server{}).publicBaseURL(r); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// X-Forwarded-Host is the more common form and keeps precedence, but a candidate
// that states the port is preferred over one that does not: the portless value
// may be the same hostname with the port dropped in transit.
func TestPublicBaseURLPrefersTheCandidateWithAPort(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "127.0.0.1:4141"
	r.Header.Set("X-Forwarded-Host", "365api.example.net")
	r.Header.Set("Forwarded", "host=365api.example.net:52325;proto=https")
	if got := (&Server{}).publicBaseURL(r); got != "https://365api.example.net:52325" {
		t.Fatalf("got %q, want https://365api.example.net:52325", got)
	}
}

// The source is what turns "the link has the wrong port" into something the
// operator can act on, so each input has to be named.
func TestPublicBaseSourceNamesTheInput(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	req := func(setup func(*http.Request)) *http.Request {
		r := httptest.NewRequest("POST", "/v1/images/generations", nil)
		r.Host = "127.0.0.1:4141"
		setup(r)
		return r
	}
	cases := []struct {
		name  string
		setup func(*http.Request)
		want  string
	}{
		{"nothing usable", func(r *http.Request) { r.Host = "example.com" }, "none"},
		{"request host", func(r *http.Request) { r.Host = "10.0.0.3:4141" }, "request-host"},
		{"x-forwarded-host", func(r *http.Request) {
			r.Header.Set("X-Forwarded-Host", "365api.example.net:52325")
		}, "x-forwarded-host"},
		{"forwarded", func(r *http.Request) {
			r.Header.Set("Forwarded", "host=365api.example.net:52325")
		}, "forwarded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (&Server{}).publicBaseSource(req(tc.setup)); got != tc.want {
				t.Fatalf("source = %q, want %q", got, tc.want)
			}
		})
	}

	t.Setenv("M365_PUBLIC_BASE_URL", "https://365api.example.net:52325")
	if got := (&Server{}).publicBaseSource(req(func(*http.Request) {})); got != "override" {
		t.Fatalf("source = %q, want override", got)
	}
}

// The log line is what the operator is asked to send back when a link points
// nowhere, so the header names in it have to be the real ones.
func TestPublicBaseDiagnosticsNamesTheInputs(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "127.0.0.1:4141"
	r.Header.Set("X-Forwarded-Host", "365api.example.net")
	r.Header.Set("X-Forwarded-Port", "52325")
	r.Header.Set("Forwarded", "host=365api.example.net:52325")

	got := (&Server{}).publicBaseDiagnostics(r)
	for _, want := range []string{
		"source=x-forwarded-host",
		`host="127.0.0.1:4141"`,
		`x-forwarded-host="365api.example.net"`,
		`x-forwarded-port="52325"`,
		`forwarded="host=365api.example.net:52325"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostics %q is missing %s", got, want)
		}
	}
}

func TestPublicBaseHasPort(t *testing.T) {
	cases := map[string]bool{
		"https://365api.example.net:52325":      true,
		"https://365api.example.net":            false,
		"https://365api.example.net:52325/m365": true,
		"https://365api.example.net/m365":       false,
		"":                                      false,
	}
	for in, want := range cases {
		if got := publicBaseHasPort(in); got != want {
			t.Errorf("publicBaseHasPort(%q) = %v, want %v", in, got, want)
		}
	}
}

// Default ports must not be appended: https://host:443/... is redundant and
// breaks certificate/origin matching in some clients.
func TestPublicBaseURLOmitsDefaultPort(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "127.0.0.1:4141"
	r.Header.Set("X-Forwarded-Host", "365api.example.net")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Port", "443")
	if got := (&Server{}).publicBaseURL(r); got != "https://365api.example.net" {
		t.Fatalf("got %q, want https://365api.example.net", got)
	}
}

// The override is the escape hatch for proxies that send a placeholder or
// rewritten Host (the reported "example.com" symptom); it must beat both the
// forwarding headers and the request Host.
func TestPublicBaseURLOverrideWinsOverForwardedHeaders(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "https://365api.example.net:52325")
	r := httptest.NewRequest("POST", "/v1/images/generations", nil)
	r.Host = "placeholder.invalid"
	r.Header.Set("X-Forwarded-Host", "placeholder.invalid")
	if got := (&Server{}).publicBaseURL(r); got != "https://365api.example.net:52325" {
		t.Fatalf("got %q, want https://365api.example.net:52325", got)
	}
}

// X-Forwarded-Host is client-supplied and ends up inside a markdown link, so a
// value that could break out of the authority component must be discarded
// rather than interpolated.
func TestPublicBaseURLRejectsMalformedForwardedHost(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	for _, bad := range []string{"evil.example.net/path", "evil.example.net foo", "evil\"onerror=x"} {
		r := httptest.NewRequest("POST", "/v1/images/generations", nil)
		r.Host = "10.0.0.3:4141"
		r.Header.Set("X-Forwarded-Host", bad)
		if got := (&Server{}).publicBaseURL(r); got != "http://10.0.0.3:4141" {
			t.Fatalf("X-Forwarded-Host=%q: got %q, want fallback http://10.0.0.3:4141", bad, got)
		}
	}
}

func TestPublicBaseURLRejectsPlaceholderHost(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	for _, placeholder := range []string{"example.com", "example.com:52325", "placeholder.invalid"} {
		r := httptest.NewRequest("POST", "/v1/images/generations", nil)
		r.Host = "10.0.0.3:4141"
		r.Header.Set("X-Forwarded-Host", placeholder)
		r.Header.Set("X-Forwarded-Proto", "https")
		if got := (&Server{}).publicBaseURL(r); got != "https://10.0.0.3:4141" {
			t.Fatalf("placeholder %q produced %q", placeholder, got)
		}
	}
}

func TestGeneratedImageURLUsesPublicBase(t *testing.T) {
	t.Setenv("M365_PUBLIC_BASE_URL", "")
	const id = "1fd01574-444a-4f60-a922-fbfd6d72ae9c"

	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Host = "127.0.0.1:4141"
	r.Header.Set("X-Forwarded-Host", "365api.example.net:52325")
	r.Header.Set("X-Forwarded-Proto", "https")
	want := "https://365api.example.net:52325/v1/images/files/" + id
	if got := (&Server{}).generatedImageURL(r, id); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Sub-path mounts must keep the prefix so the proxy can route the file back.
	t.Setenv("M365_PUBLIC_BASE_URL", "https://api.example.net/m365")
	want = "https://api.example.net/m365/v1/images/files/" + id
	if got := (&Server{}).generatedImageURL(r, id); got != want {
		t.Fatalf("sub-path: got %q, want %q", got, want)
	}
}

func TestSettingsRejectInvalidPublicBaseURL(t *testing.T) {
	cfg := defaultRuntimeSettings()
	cfg.PublicBaseURL = "ftp://api.example.net"
	if err := validateSettings(cfg); err == nil {
		t.Fatal("expected an error for a non-http(s) public base URL")
	}
	cfg.PublicBaseURL = "https://365api.example.net:52325"
	if err := validateSettings(cfg); err != nil {
		t.Fatalf("valid public base URL rejected: %v", err)
	}
}

// End-to-end: the URL handed to the client must actually resolve back to the
// stored bytes. A base that is merely well-formed is not enough — the path
// component has to stay routable after the public prefix is prepended.
func TestGeneratedImageURLRoundTripsThroughHandler(t *testing.T) {
	t.Setenv("M365_DATA_DIR", t.TempDir())
	t.Setenv("M365_PUBLIC_BASE_URL", "https://365api.example.net:52325")

	data := testStoredPNG(t)
	srv := &Server{}
	id, err := srv.storeGeneratedImage(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Host = "127.0.0.1:4141"
	link := srv.generatedImageURL(req, id)
	if !strings.HasPrefix(link, "https://365api.example.net:52325/v1/images/files/") {
		t.Fatalf("unexpected link %q", link)
	}

	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	srv.generatedImageFile(rr, httptest.NewRequest("GET", u.Path, nil))
	if rr.Code != 200 || !bytes.Equal(rr.Body.Bytes(), data) {
		t.Fatalf("fetching the advertised URL failed: status=%d len=%d", rr.Code, rr.Body.Len())
	}
}
