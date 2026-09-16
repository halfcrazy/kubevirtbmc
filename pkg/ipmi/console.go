package ipmi

import (
	"context"
	"net"
	"sync"
	"sync/atomic"

	"github.com/bougou/go-ipmi/pkg/hal"

	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
)

// consoleReadBuf is the per-read chunk size of the console pump.
const consoleReadBuf = 4096

// consoleReadChunks bounds console output buffered between SOL data-plane
// drains. A full channel applies backpressure to the guest's console writer
// (the pump stops reading, TCP flow control stalls the websocket) instead of
// dropping bytes — the in-tree equivalent of the BMC deasserting CTS
// (spec v2.0 §15.6).
const consoleReadChunks = 16

// vmConsoleHAL exposes the VM's serial console to go-ipmi's SOL payload
// support through the hal.ConsoleHAL contract.
type vmConsoleHAL struct {
	rm resourcemanager.ResourceManager
}

// Open attaches to the VM serial console (spec §24.1 Activate Payload).
// Single-attachment semantics (spec §15.3) are enforced by the resource
// manager: a second Open fails while the first stream is alive.
func (h vmConsoleHAL) Open(ctx context.Context) (hal.ConsoleConn, error) {
	conn, err := h.rm.OpenConsole(ctx)
	if err != nil {
		return nil, err
	}
	return newConsoleConn(conn), nil
}

// consoleConn adapts a blocking net.Conn (the KubeVirt serial-console
// websocket) to hal.ConsoleConn.
//
// The SOL data plane drains console output synchronously while answering
// remote-console packets, so ReadAvailable must never block (hal.ConsoleConn
// contract). Firing a read deadline on a websocket is not an option — per
// gorilla/websocket the connection state is corrupt after a read timeout —
// so a dedicated pump goroutine owns all reads and feeds a bounded buffer
// that ReadAvailable drains.
type consoleConn struct {
	conn net.Conn

	chunks    chan []byte   // console output pending consumption
	closeCh   chan struct{} // closed by Close to unblock a backpressured pump
	closeOnce sync.Once
	pumpErr   atomic.Value // sticky error from the read pump

	mu  sync.Mutex
	cur []byte // remainder of the chunk being drained
}

func newConsoleConn(conn net.Conn) *consoleConn {
	c := &consoleConn{
		conn:    conn,
		chunks:  make(chan []byte, consoleReadChunks),
		closeCh: make(chan struct{}),
	}
	go c.pump()
	return c
}

// pump is the only reader of the underlying conn. It exits on the first read
// error (peer close, Close() unblocking a pending read, websocket failure);
// the error is sticky so ReadAvailable reports the broken console once the
// buffered output is drained.
func (c *consoleConn) pump() {
	buf := make([]byte, consoleReadBuf)
	for {
		n, err := c.conn.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case c.chunks <- chunk:
			case <-c.closeCh:
				return
			}
		}
		if err != nil {
			c.pumpErr.Store(err)
			return
		}
	}
}

// ReadAvailable copies immediately-pending console output into p, returning
// (0, nil) when nothing is waiting. Buffered output is delivered before the
// pump's sticky error so the guest's final bytes are not lost.
func (c *consoleConn) ReadAvailable(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.cur) == 0 {
		select {
		case c.cur = <-c.chunks:
		default:
			if err, ok := c.pumpErr.Load().(error); ok {
				return 0, err
			}
			return 0, nil
		}
	}
	n := copy(p, c.cur)
	c.cur = c.cur[n:]
	return n, nil
}

func (c *consoleConn) Write(p []byte) (int, error) {
	return c.conn.Write(p)
}

func (c *consoleConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closeCh)
		err = c.conn.Close()
	})
	return err
}

// SendBreak returns hal.ErrNotSupported: the websocket transport to the
// KubeVirt console subresource has no serial BREAK concept.
func (c *consoleConn) SendBreak(context.Context) error {
	return hal.ErrNotSupported
}
