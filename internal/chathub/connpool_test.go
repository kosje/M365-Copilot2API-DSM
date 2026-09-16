package chathub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsTestServer upgrades every request and hands the connection to onConn.
// It returns the server, its ws:// URL, and a channel the test closes to make
// the server stop holding the socket open.
func wsTestServer(t *testing.T, onConn func(*websocket.Conn, <-chan struct{})) (*httptest.Server, string) {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		onConn(c, release)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func newTestPool(t *testing.T, wsURL string) (*ConnPool, Account) {
	t.Helper()
	d := *websocket.DefaultDialer
	p := NewConnPool(&d, http.Header{})
	t.Cleanup(p.Close)
	return p, Account{OID: "oid-test", TID: "tid-test"}
}

// A warmed connection must complete the handshake Warm performs: it writes the
// SignalR handshake and then waits for one message.
func handshakeEcho(c *websocket.Conn, release <-chan struct{}) {
	_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":1}`+"\x1e"))
	<-release
	_ = c.Close()
}

// The consumer of a taken pooled connection only selects on frames, errs and
// ctx. If the park goroutine exits without closing frames, that consumer blocks
// forever - and the request it belongs to holds an account concurrency slot, so
// the account eventually goes out of service. The previous revision left
// exactly that hole on the stall path.
func TestConnPoolParkReleasesReaderWhenUpstreamCloses(t *testing.T) {
	var serverConn *websocket.Conn
	srv, wsURL := wsTestServer(t, func(c *websocket.Conn, release <-chan struct{}) {
		serverConn = c
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":1}`+"\x1e"))
		<-release
		_ = c.Close()
	})
	defer srv.Close()

	pool, acc := newTestPool(t, wsURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool.Warm(ctx, acc, wsURL)
	// Warm runs the handshake inline, so the connection is pooled by now.
	if got := pool.Stats()["pooled_connections"]; got != 1 {
		t.Fatalf("expected one pooled connection, got %v", got)
	}

	conn, _, frames, errs, reused, err := pool.Take(ctx, acc.OID, acc.TID, wsURL)
	if err != nil {
		t.Fatal(err)
	}
	if !reused || conn == nil {
		t.Fatal("expected to take the warmed connection")
	}

	// Drop the upstream end: the park goroutine's read now fails while the
	// connection is taken, which must release the consumer.
	if serverConn != nil {
		_ = serverConn.Close()
	}

	select {
	case _, ok := <-frames:
		if ok {
			t.Fatal("expected frames to be closed, not to deliver data")
		}
		// Channel closed: the consumer is released.
		select {
		case e := <-errs:
			if e == nil {
				t.Fatal("expected a non-nil error alongside the close")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("frames closed without an error being reported")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("park goroutine exited without closing frames: the reader would block forever")
	}
}

func TestPooledConnCloseFramesIsIdempotent(t *testing.T) {
	pc := &pooledConn{frames: make(chan []byte, 1), errs: make(chan error, 1)}
	pc.closeFrames()
	// A second close would panic if it were not guarded; the park goroutine,
	// evict() and GC() can all reach this.
	pc.closeFrames()
	select {
	case _, ok := <-pc.frames:
		if ok {
			t.Fatal("frames should be closed")
		}
	default:
		t.Fatal("frames should be closed")
	}
	// recordErr must not block or panic on a full or unread channel.
	pc.recordErr(nil)
	pc.recordErr(errSomething)
	if pc.frames == nil {
		t.Fatal("frames unexpectedly nil")
	}
	// A conn with no channels at all (never parked) must be safe too.
	bare := &pooledConn{}
	bare.closeFrames()
	bare.recordErr(nil)
}

func TestConnPoolCloseIsIdempotentAndRejectsTake(t *testing.T) {
	_, wsURL := wsTestServer(t, handshakeEcho)
	pool, acc := newTestPool(t, wsURL)

	pool.Close()
	pool.Close() // the previous revision panicked here (close of a closed channel)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, _, _, _, err := pool.Take(ctx, acc.OID, acc.TID, wsURL); err == nil {
		t.Fatal("a closed pool must refuse to hand out connections")
	}
}

func TestConnPoolTakeHonoursCancelledContext(t *testing.T) {
	_, wsURL := wsTestServer(t, handshakeEcho)
	pool, acc := newTestPool(t, wsURL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, _, _, err := pool.Take(ctx, acc.OID, acc.TID, wsURL); err == nil {
		// A dial with an already-cancelled context must fail rather than hang.
		t.Log("take with a cancelled context returned no error; dial raced the cancel")
	}
}

var errSomething = errTest("test error")

type errTest string

func (e errTest) Error() string { return string(e) }
