package web

import (
	"errors"
	"io"
	"strings"
)

// isUpstreamConnectionDrop reports whether err looks like a transport-level
// disconnect from upstream (as opposed to a clean application-level error).
// It is used to decide whether a mid-stream failure can be transparently
// retried on another healthy account before any content reached the client.
//
// Context cancellations and deadlines are deliberately excluded: those are
// usually our own chat timeout firing, not an upstream drop, and retrying them
// on another account is futile.
func isUpstreamConnectionDrop(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"connection reset",
		"broken pipe",
		"connection refused",
		"i/o timeout",
		"unexpected eof",
		"server closed",
		"tls handshake",
		"net/http: stream",
		"http2: stream",
		"connection closed",
		"reset by peer",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
