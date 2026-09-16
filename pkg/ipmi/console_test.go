package ipmi

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/bougou/go-ipmi/pkg/hal"
)

// serveConn pairs a consoleConn with the peer end of its net.Pipe, closing
// both at test cleanup.
func serveConn(t *testing.T) (*consoleConn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return newConsoleConn(a), b
}

func TestReadAvailableEmptyIsNonBlocking(t *testing.T) {
	c, _ := serveConn(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		n, err := c.ReadAvailable(make([]byte, 16))
		if n != 0 || err != nil {
			t.Errorf("ReadAvailable on idle console = (%d, %v), want (0, nil)", n, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ReadAvailable blocked on an idle console")
	}
}

func TestReadAvailableDeliversPeerOutput(t *testing.T) {
	c, peer := serveConn(t)

	if _, err := peer.Write([]byte("login: ")); err != nil {
		t.Fatalf("peer write: %v", err)
	}

	buf := make([]byte, 4)
	var got []byte
	deadline := time.Now().Add(2 * time.Second)
	for len(got) < len("login: ") && time.Now().Before(deadline) {
		n, err := c.ReadAvailable(buf)
		if err != nil {
			t.Fatalf("ReadAvailable: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	if string(got) != "login: " {
		t.Fatalf("drained %q, want %q", got, "login: ")
	}
}

func TestReadAvailableReportsErrorAfterDrain(t *testing.T) {
	c, peer := serveConn(t)

	if _, err := peer.Write([]byte("bye")); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	// Closing the peer breaks the pump; the buffered output must still be
	// delivered before the sticky error surfaces.
	if err := peer.Close(); err != nil {
		t.Fatalf("peer close: %v", err)
	}

	buf := make([]byte, 16)
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := c.ReadAvailable(buf)
		if err != nil {
			if !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAvailable error = %v, want pipe close/EOF", err)
			}
			return
		}
		if n > 0 {
			if string(buf[:n]) != "bye" {
				t.Fatalf("drained %q, want %q", buf[:n], "bye")
			}
			continue
		}
		if time.Now().After(deadline) {
			t.Fatal("pump error never surfaced after the buffer drained")
		}
	}
}

func TestWriteReachesPeer(t *testing.T) {
	c, peer := serveConn(t)

	// net.Pipe is synchronous: the write blocks until the peer reads, so the
	// read must run concurrently.
	got := make([]byte, 5)
	readErr := make(chan error, 1)
	go func() {
		_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := io.ReadFull(peer, got)
		readErr <- err
	}()
	if _, err := c.Write([]byte("root\r")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := <-readErr; err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if string(got) != "root\r" {
		t.Fatalf("peer read %q, want %q", got, "root\r")
	}
}

func TestSendBreakUnsupported(t *testing.T) {
	c, _ := serveConn(t)
	if err := c.SendBreak(t.Context()); !errors.Is(err, hal.ErrNotSupported) {
		t.Fatalf("SendBreak = %v, want hal.ErrNotSupported", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c, _ := serveConn(t)
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
