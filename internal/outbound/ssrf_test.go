package outbound

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsUnsafeIP(t *testing.T) {
	cases := []struct {
		ip     string
		unsafe bool
	}{
		{"127.0.0.1", true},
		{"127.1.2.3", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"10.0.0.5", true},
		{"172.16.3.4", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"169.254.1.1", true},     // link-local
		{"100.64.0.1", true},      // CGNAT
		{"100.127.255.254", true}, // CGNAT upper edge
		{"224.0.0.1", true},       // multicast
		{"ff02::1", true},         // multicast v6
		{"::ffff:127.0.0.1", true},
		{"0.1.2.3", true}, // 0.0.0.0/8
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"52.113.194.132", false}, // an actual Microsoft address
		{"2606:4700:4700::1111", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("test case %q is not parseable", c.ip)
		}
		if got := IsUnsafeIP(ip); got != c.unsafe {
			t.Errorf("IsUnsafeIP(%s) = %v, want %v", c.ip, got, c.unsafe)
		}
	}
	// A nil address must not be dialled.
	if !IsUnsafeIP(nil) {
		t.Error("IsUnsafeIP(nil) = false, want true")
	}
}

// The untrusted-fetch client must refuse private addresses at dial time. This is
// the check hostname validation cannot do: it sees the address actually being
// connected to, after resolution and after every redirect.
func TestUntrustedClientRefusesPrivateAddresses(t *testing.T) {
	// The proxy configuration is package-level state; start from a known
	// direct-dial state so the assertion does not depend on test order.
	if err := Configure(""); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not be reachable"))
	}))
	defer srv.Close()

	client := UntrustedHTTPClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("the untrusted-fetch client reached %s, which is loopback", srv.URL)
	}
	if !IsBlockedAddress(err) {
		t.Fatalf("expected a blocked-address error, got %v", err)
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("error should explain the refusal, got %v", err)
	}

	// The escape hatch must work, so an operator who deliberately fronts their
	// upstream with a private address is not stuck.
	t.Setenv(EnvAllowPrivateOutbound, "1")
	resp2, err := UntrustedHTTPClient(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("override did not take effect: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("override request status = %d", resp2.StatusCode)
	}
}

// The shared client serves operator-configured endpoints - the OAuth authority,
// the upstream gateway - which may legitimately be private (an on-prem ADFS, a
// local stub during development). The guard therefore must NOT be installed
// there: doing so broke the PKCE callback flow's own test.
func TestSharedClientStillReachesConfiguredPrivateEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("configured endpoint"))
	}))
	defer srv.Close()

	resp, err := HTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("the shared client must still reach a configured endpoint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// A caller that was handed its own transport must keep using it.
func TestUntrustedClientFromKeepsInjectedTransport(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	t.Setenv(EnvAllowPrivateOutbound, "1")
	// An injected RoundTripper that is not an *http.Transport must be passed
	// through unchanged.
	injected := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})}
	c := UntrustedClientFrom(injected, time.Second)
	if _, ok := c.Transport.(roundTripFunc); !ok {
		t.Fatalf("injected non-transport RoundTripper was replaced: %T", c.Transport)
	}
	if reached {
		t.Fatal("unexpected")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDialControlAllowsPublicLiteral(t *testing.T) {
	if err := DialControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Fatalf("public literal was refused: %v", err)
	}
	if err := DialControl("tcp", "[2606:4700:4700::1111]:443", nil); err != nil {
		t.Fatalf("public v6 literal was refused: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:4141", "[::1]:4141", "192.168.0.10:80", "169.254.169.254:80"} {
		if err := DialControl("tcp", addr, nil); err == nil {
			t.Errorf("DialControl allowed %s", addr)
		} else if !IsBlockedAddress(err) {
			t.Errorf("DialControl(%s) returned %v, want a blocked-address error", addr, err)
		}
	}
}
