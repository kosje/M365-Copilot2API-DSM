package chathub

import (
	"fmt"
	"net"
	"net/url"

	"m365-copilot2api/internal/outbound"
)

// ValidateRemoteDownloadURL blocks SSRF on URLs derived from a model response:
// only https and public routable addresses are accepted, with a lookup-time
// recheck against private, loopback, link-local and cloud metadata ranges.
//
// This is a pre-flight check, and on its own it is racy - a hostname can resolve
// to a public address here and to 127.0.0.1 for the connection that follows
// (DNS rebinding). outbound.DialControl is what actually closes that gap, by
// re-checking the address being dialled; this function exists to fail early with
// a clear message.
func ValidateRemoteDownloadURL(raw string) error {
	return validateRemoteDownloadURL(raw)
}

func validateRemoteDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid attachment URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("attachment download requires https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("attachment URL has no host")
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("attachment host does not resolve")
	}
	for _, ip := range ips {
		if ipUnsafe(ip) {
			return fmt.Errorf("attachment URL targets a non-public address")
		}
	}
	return nil
}

// ipUnsafe delegates to outbound so the dialer, the web layer and this check all
// apply one definition of "not safe to fetch". (169.254.169.254 is link-local
// and covered there.)
func ipUnsafe(ip net.IP) bool {
	return outbound.IsUnsafeIP(ip)
}
