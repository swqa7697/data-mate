package mysql

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// setupReceiveBudget bounds handshake, privilege audit and session setup
// traffic. User queries and catalog reads have separate encoded-result limits.
const setupReceiveBudget = 8 << 20

var errReceiveBudget = errors.New("receive budget exceeded")

// wire observes the dialed connection beneath TLS with constant memory. It
// never inspects payloads; each physical connection owns one observer.
type wire struct {
	net.Conn
	remaining atomic.Int64
	greeted   atomic.Bool
	exceeded  atomic.Bool
	done      chan struct{}
	once      sync.Once
}

func newWire(c net.Conn) *wire {
	w := &wire{Conn: c, done: make(chan struct{})}
	w.remaining.Store(setupReceiveBudget)
	return w
}

// reset bounds the next setup or cleanup phase.
func (w *wire) reset() { w.remaining.Store(setupReceiveBudget) }

// readResults leaves result sizes to the profile and the server.
func (w *wire) readResults() { w.remaining.Store(-1) }

func (w *wire) Read(p []byte) (int, error) {
	n, err := w.Conn.Read(p)
	if n > 0 {
		// The server speaks first, so any received byte is its greeting.
		w.greeted.Store(true)
		if w.remaining.Load() >= 0 && w.remaining.Add(-int64(n)) < 0 {
			w.exceeded.Store(true)
			_ = w.Close()
			return 0, errReceiveBudget
		}
	}
	return n, err
}

// Close closes the route, including any SSH tunnel, exactly once.
func (w *wire) Close() error {
	var err error
	w.once.Do(func() {
		err = w.Conn.Close()
		close(w.done)
	})
	return err
}

func (w *wire) closed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// bound limits blocking reads and writes during cleanup and close.
func (w *wire) bound(d time.Duration) { _ = w.Conn.SetDeadline(time.Now().Add(d)) }
