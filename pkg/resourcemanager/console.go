package resourcemanager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

// ErrConsoleBusy is returned by OpenConsole when the VM's serial console is
// already attached. A VM has a single serial port, so it cannot serve two
// streams (IPMI spec v2.0 §15.3 serial port sharing, surfaced through
// hal.ConsoleHAL).
var ErrConsoleBusy = errors.New("serial console already attached")

// ConsoleOpener opens a bidirectional byte stream to a VM's serial console.
// Production wiring dials the KubeVirt serial-console subresource (the same
// API behind `virtctl console`); tests inject fakes.
type ConsoleOpener func(ctx context.Context, namespace, name string) (net.Conn, error)

// WithConsoleOpener configures the serial-console backend used by OpenConsole.
func WithConsoleOpener(opener ConsoleOpener) Option {
	return func(m *VirtualMachineResourceManager) {
		m.consoleOpener = opener
	}
}

// OpenConsole attaches to the VM's serial console and returns its byte
// stream. The console is a single-attachment resource: a second call fails
// with ErrConsoleBusy until the first stream is closed — closing the returned
// conn releases the slot.
func (m *VirtualMachineResourceManager) OpenConsole(ctx context.Context) (net.Conn, error) {
	if m.consoleOpener == nil {
		return nil, fmt.Errorf("serial console is not configured")
	}

	m.consoleMu.Lock()
	if m.consoleBusy {
		m.consoleMu.Unlock()
		return nil, ErrConsoleBusy
	}
	conn, err := m.consoleOpener(ctx, m.namespace, m.name)
	if err != nil {
		m.consoleMu.Unlock()
		return nil, err
	}
	m.consoleBusy = true
	m.consoleMu.Unlock()

	return &consoleGuardConn{
		Conn: conn,
		onClose: func() {
			m.consoleMu.Lock()
			m.consoleBusy = false
			m.consoleMu.Unlock()
		},
	}, nil
}

// consoleGuardConn releases the resource manager's console slot when closed.
type consoleGuardConn struct {
	net.Conn
	onClose func()
	once    sync.Once
}

func (c *consoleGuardConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.onClose()
	})
	return err
}
