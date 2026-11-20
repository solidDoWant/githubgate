package server

import (
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

// progressListener wraps each accepted connection so that it closes after
// idle with no bytes moving in either direction. Unlike a read or write
// timeout, it never cuts off a transfer that keeps moving, and it covers
// every read and write of the connection, including the TLS handshake and
// the bodies a handler reads.
type progressListener struct {
	net.Listener

	idle time.Duration
}

func newProgressListener(listener net.Listener, idle time.Duration) net.Listener {
	return &progressListener{Listener: listener, idle: idle}
}

func (l *progressListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &progressConn{Conn: conn, idle: l.idle, last: time.Now()}, nil
}

// progressConn tracks the time of the last byte moved in either direction.
// The deadlines the HTTP server sets for itself still apply.
type progressConn struct {
	net.Conn

	idle time.Duration

	mu           sync.Mutex
	last         time.Time
	readDeadline time.Time
}

func (c *progressConn) touch() {
	c.mu.Lock()
	c.last = time.Now()
	c.mu.Unlock()
}

// limits returns the last progress time and the deadline the caller set.
func (c *progressConn) limits() (time.Time, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.last, c.readDeadline
}

func (c *progressConn) Read(buffer []byte) (int, error) {
	for {
		last, requested := c.limits()

		deadline := last.Add(c.idle)
		if !requested.IsZero() && requested.Before(deadline) {
			deadline = requested
		}

		if err := c.Conn.SetReadDeadline(deadline); err != nil {
			return 0, err
		}

		count, err := c.Conn.Read(buffer)
		if count > 0 {
			c.touch()
		}

		if count == 0 && errors.Is(err, os.ErrDeadlineExceeded) {
			// A write may have moved bytes while this read waited, and the
			// caller's own deadline may have changed.
			last, requested = c.limits()
			now := time.Now()

			if now.Before(last.Add(c.idle)) && (requested.IsZero() || now.Before(requested)) {
				continue
			}
		}

		return count, err
	}
}

func (c *progressConn) Write(data []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}

	count, err := c.Conn.Write(data)
	if count > 0 {
		c.touch()
	}

	return count, err
}

// SetReadDeadline records the caller's deadline; Read combines it with the
// progress deadline. It also passes the deadline on, so a blocked Read wakes
// when the caller aborts it.
func (c *progressConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	c.mu.Unlock()

	return c.Conn.SetReadDeadline(deadline)
}

// SetDeadline applies the caller's deadline to reads; writes are bounded by
// progress alone.
func (c *progressConn) SetDeadline(deadline time.Time) error {
	return c.SetReadDeadline(deadline)
}

// SetWriteDeadline is ignored: Write bounds itself by the progress timeout.
func (c *progressConn) SetWriteDeadline(time.Time) error {
	return nil
}
