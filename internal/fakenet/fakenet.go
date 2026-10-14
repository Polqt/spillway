// Package fakenet provides an in-memory network for deterministic HTTP tests.
package fakenet

import (
	"context"
	"net"
	"sync"
)

// Listener is an in-memory listener whose connections use net.Pipe.
type Listener struct {
	mu     sync.Mutex
	closed bool
	conns  chan net.Conn
	done   chan struct{}
}

// Listen creates an in-memory listener.
func Listen() *Listener {
	return &Listener{conns: make(chan net.Conn), done: make(chan struct{})}
}

// Accept waits for a connection created by DialContext.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close prevents new connections and unblocks pending accepts and dials.
func (l *Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.done)
	}
	return nil
}

// Addr returns an in-memory address.
func (l *Listener) Addr() net.Addr { return memoryAddr("fakenet") }

// DialContext creates one end of a pipe and queues the other for Accept.
func (l *Listener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	client, server := net.Pipe()
	l.mu.Unlock()
	select {
	case l.conns <- server:
		return client, nil
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	case <-l.done:
		_ = client.Close()
		_ = server.Close()
		return nil, net.ErrClosed
	}
}

type memoryAddr string

func (a memoryAddr) Network() string { return string(a) }
func (a memoryAddr) String() string  { return string(a) }

var _ net.Listener = (*Listener)(nil)
