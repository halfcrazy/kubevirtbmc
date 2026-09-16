package ipmi

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	goipmi "github.com/bougou/go-ipmi/pkg/client"
	"go.uber.org/mock/gomock"

	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
)

// lockedBuffer is an io.Writer safe for concurrent reads from the test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForCondition(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeGuest plays the guest OS end of the serial console. net.Pipe is
// synchronous — a write (even a zero-length one) blocks until the peer
// reads — so a drain goroutine must always consume the keystroke direction,
// like the guest kernel's UART driver would.
type fakeGuest struct {
	conn net.Conn // the test writes guest output here
	rx   lockedBuffer
}

func newFakeGuest(t *testing.T, conn net.Conn) *fakeGuest {
	g := &fakeGuest{conn: conn}
	t.Cleanup(func() { _ = conn.Close() })
	go func() { _, _ = io.Copy(&g.rx, conn) }()
	return g
}

func (g *fakeGuest) WriteOutput(t *testing.T, s string) {
	t.Helper()
	if _, err := g.conn.Write([]byte(s)); err != nil {
		t.Fatalf("guest output write: %v", err)
	}
}

func (g *fakeGuest) ReceivedKeystrokes() string { return g.rx.String() }

// newSOLSimulator starts the real simulator on a UDP loopback port with the
// console backed by net.Pipe pairs — one per OpenConsole call — and returns a
// connected lanplus client plus the channel of guest ends.
func newSOLSimulator(t *testing.T) (client *goipmi.Client, guests chan *fakeGuest) {
	t.Helper()

	// Each OpenConsole (initial activation, re-activation) gets a fresh pipe.
	guests = make(chan *fakeGuest, 4)
	ctrl := gomock.NewController(t)
	rm := resourcemanager.NewMockResourceManager(ctrl)
	rm.EXPECT().GetSystemUUID(gomock.Any()).Return("ec2b8f0e-4c1a-4b6d-9f3a-1e5c7a0d2b4f", nil).AnyTimes()
	rm.EXPECT().OpenConsole(gomock.Any()).DoAndReturn(func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close() })
		guests <- newFakeGuest(t, b)
		return a, nil
	}).AnyTimes()

	sim := NewSimulator("127.0.0.1", 0, rm, "admin", "secret")
	if err := sim.Run(); err != nil {
		t.Fatalf("simulator run: %v", err)
	}
	t.Cleanup(sim.Stop)

	addr := sim.LocalAddr().(*net.UDPAddr)
	client, err := goipmi.NewClient(addr.IP.String(), addr.Port, "admin", "secret")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.WithInterface(goipmi.InterfaceLanplus)
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client, guests
}

// nextGuest waits for the guest end of the next OpenConsole pipe.
func nextGuest(t *testing.T, guests chan *fakeGuest) *fakeGuest {
	t.Helper()
	select {
	case g := <-guests:
		return g
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for console attach")
		return nil
	}
}

// TestSOLSessionLoopback runs a full SOL session against the simulator over
// UDP loopback: activation, console output to the remote side, keystrokes to
// the console, clean deactivation on input EOF, and re-activation.
func TestSOLSessionLoopback(t *testing.T) {
	c, guests := newSOLSimulator(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inR, inW := io.Pipe()
	out := &lockedBuffer{}

	// Poll slower than the server's 50ms retry interval (§15.11) so the retry
	// engine fires between polls — mirrors go-ipmi's own loopback test.
	solErr := make(chan error, 1)
	go func() {
		solErr <- c.SOLActivate(ctx, inR, out, &goipmi.SOLActivateOptions{PollInterval: 100 * time.Millisecond})
	}()

	guest := nextGuest(t, guests)

	// Guest→remote: bytes on the VM serial port reach the remote console.
	guest.WriteOutput(t, "login: ")
	waitForCondition(t, "console output at remote", func() bool {
		return strings.Contains(out.String(), "login: ")
	})

	// Remote→guest: keystrokes from the remote console land on the serial port.
	if _, err := inW.Write([]byte("root\r")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	waitForCondition(t, "keystrokes at console", func() bool {
		return strings.Contains(guest.ReceivedKeystrokes(), "root\r")
	})

	// Input EOF ends the session and deactivates the payload, which closes the
	// console stream.
	_ = inW.Close()
	select {
	case err := <-solErr:
		if err != nil {
			t.Fatalf("SOLActivate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SOLActivate did not return after input EOF")
	}

	// The payload is released: a fresh session can activate SOL again and gets
	// a new console stream.
	inR2, inW2 := io.Pipe()
	defer func() { _ = inW2.Close() }()
	out2 := &lockedBuffer{}
	go func() {
		solErr <- c.SOLActivate(ctx, inR2, out2, &goipmi.SOLActivateOptions{PollInterval: 100 * time.Millisecond})
	}()
	guest2 := nextGuest(t, guests)
	guest2.WriteOutput(t, "again")
	waitForCondition(t, "console output after re-activation", func() bool {
		return strings.Contains(out2.String(), "again")
	})
}

// TestSOLSecondActivationRejected verifies single-instance semantics
// (spec §15.3): a concurrent `sol activate` fails while a session owns the
// payload.
func TestSOLSecondActivationRejected(t *testing.T) {
	c, guests := newSOLSimulator(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inR, inW := io.Pipe()
	defer func() { _ = inW.Close() }()
	out := &lockedBuffer{}
	solErr := make(chan error, 1)
	go func() {
		solErr <- c.SOLActivate(ctx, inR, out, &goipmi.SOLActivateOptions{PollInterval: 100 * time.Millisecond})
	}()
	guest := nextGuest(t, guests)
	guest.WriteOutput(t, "x")
	waitForCondition(t, "first session receiving output", func() bool {
		return strings.Contains(out.String(), "x")
	})

	// A second client on the same BMC must be refused while the payload is
	// owned. go-ipmi's client is single-session, so open a second connection.
	c2, err := goipmi.NewClient(c.Host, c.Port, "admin", "secret")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c2.WithInterface(goipmi.InterfaceLanplus)
	if err := c2.Connect(context.Background()); err != nil {
		t.Fatalf("second client connect: %v", err)
	}
	defer func() { _ = c2.Close(context.Background()) }()

	out2 := &lockedBuffer{}
	err = c2.SOLActivate(ctx, strings.NewReader(""), out2, &goipmi.SOLActivateOptions{PollInterval: 100 * time.Millisecond})
	if err == nil {
		t.Fatal("second SOLActivate succeeded, want already-active failure")
	}
	if strings.Contains(out2.String(), "x") {
		t.Fatal("second session received console output owned by the first")
	}
}
