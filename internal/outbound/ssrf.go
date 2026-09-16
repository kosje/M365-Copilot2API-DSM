package outbound

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
)

// EnvAllowPrivateOutbound disables the dial-time guard below.
const EnvAllowPrivateOutbound = "M365_ALLOW_PRIVATE_OUTBOUND"

// allowPrivateOutbound reports whether the operator has explicitly opted out of
// the private-address guard.
func allowPrivateOutbound() bool {
	v := strings.TrimSpace(os.Getenv(EnvAllowPrivateOutbound))
	return v == "1" || strings.EqualFold(v, "true")
}

// IsUnsafeIP reports whether ip must not be dialled on behalf of a
// user-supplied URL or a response-derived link.
//
// Validating by resolving the hostname and then dialling separately is
// bypassable: an attacker-controlled name can resolve to a public address for
// the check and to 127.0.0.1 for the connection (DNS rebinding). The only place
// the check cannot be raced is at dial time, against the address actually being
// connected to - see DialControl.
func IsUnsafeIP(ip net.IP) bool {
	if ip == nil {
		// An address that does not parse is not something to dial.
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 100.64.0.0/10 (CGNAT) is not covered by IP.IsPrivate. 169.254.0.0/16
		// is covered, by the link-local check above.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
		// 0.0.0.0/8, which some stacks route to the local host.
		if ip4[0] == 0 {
			return true
		}
	}
	// IPv4-mapped forms of the above (::ffff:127.0.0.1) are caught by To4();
	// :: and ::1 by IsUnspecified and IsLoopback.
	return false
}

// blockedAddressError is returned when a dial is refused by DialControl.
//
// The message deliberately does not name the override variable: responses pass
// through the public-identity rewriting layer, which substitutes the product
// name and turned the variable name into nonsense in an earlier draft. The
// override is documented in .env.example and SECURITY.md instead.
type blockedAddressError struct{ addr string }

func (e *blockedAddressError) Error() string {
	return fmt.Sprintf("refusing to connect to non-public address %s", e.addr)
}

// IsBlockedAddress reports whether err came from DialControl refusing an
// address, so callers can distinguish "we refused" from "the network failed".
func IsBlockedAddress(err error) bool {
	var b *blockedAddressError
	return errors.As(err, &b)
}

// DialControl is installed on the direct dialer so that every connection is
// checked against the address actually being used - after DNS resolution and
// after any redirect. Upstream Microsoft endpoints are unaffected: they all
// resolve to public addresses.
//
// The proxy paths (http/https/socks5) replace DialContext entirely, so this does
// not apply there; a configured proxy is trusted to reach the target. That is
// also the supported escape hatch for anyone who genuinely needs the gateway to
// reach a private address.
func DialControl(network, address string, _ syscall.RawConn) error {
	if allowPrivateOutbound() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// Not an IP literal. After resolution this should not happen; allow it
		// rather than break dialling on an unusual address form.
		return nil
	}
	if IsUnsafeIP(ip) {
		return &blockedAddressError{addr: address}
	}
	return nil
}
