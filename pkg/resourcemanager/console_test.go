package resourcemanager

import (
	"context"
	"errors"
	"net"
	"testing"
)

func openTestConsole(t *testing.T, m *VirtualMachineResourceManager) net.Conn {
	t.Helper()
	conn, err := m.OpenConsole(context.Background())
	if err != nil {
		t.Fatalf("OpenConsole: %v", err)
	}
	return conn
}

func TestOpenConsoleRequiresOpener(t *testing.T) {
	m := &VirtualMachineResourceManager{}
	if _, err := m.OpenConsole(context.Background()); err == nil {
		t.Fatal("OpenConsole without opener succeeded, want error")
	}
}

func TestOpenConsoleSingleAttachment(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()

	m := &VirtualMachineResourceManager{}
	WithConsoleOpener(func(context.Context, string, string) (net.Conn, error) { return a, nil })(m)

	first := openTestConsole(t, m)

	if _, err := m.OpenConsole(context.Background()); !errors.Is(err, ErrConsoleBusy) {
		t.Fatalf("second OpenConsole = %v, want ErrConsoleBusy", err)
	}

	// Closing the stream releases the console slot.
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second := openTestConsole(t, m)
	if err := second.Close(); err != nil {
		t.Fatalf("close second: %v", err)
	}
}

func TestOpenConsoleOpenerErrorLeavesSlotFree(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()

	fail := true
	m := &VirtualMachineResourceManager{}
	WithConsoleOpener(func(context.Context, string, string) (net.Conn, error) {
		if fail {
			return nil, errors.New("dial failed")
		}
		return a, nil
	})(m)

	if _, err := m.OpenConsole(context.Background()); err == nil {
		t.Fatal("OpenConsole with failing opener succeeded, want error")
	}
	fail = false
	conn := openTestConsole(t, m)
	_ = conn.Close()
}

func TestOpenConsolePassesNamespaceAndName(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()

	var gotNS, gotName string
	m := &VirtualMachineResourceManager{namespace: "ns1", name: "vm1"}
	WithConsoleOpener(func(_ context.Context, namespace, name string) (net.Conn, error) {
		gotNS, gotName = namespace, name
		return a, nil
	})(m)

	conn := openTestConsole(t, m)
	defer func() { _ = conn.Close() }()
	if gotNS != "ns1" || gotName != "vm1" {
		t.Fatalf("opener got (%q, %q), want (%q, %q)", gotNS, gotName, "ns1", "vm1")
	}
}
