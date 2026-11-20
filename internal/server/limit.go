package server

import (
	"net"
	"sync"
)

// limitListener wraps a listener and closes a new connection at once when
// limit connections are already open, so that a client holding many idle
// connections cannot use up the server's memory.
type limitListener struct {
	net.Listener

	slots chan struct{}
}

func newLimitListener(listener net.Listener, limit int) net.Listener {
	return &limitListener{Listener: listener, slots: make(chan struct{}, limit)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		select {
		case l.slots <- struct{}{}:
			return &limitedConn{Conn: conn, release: func() { <-l.slots }}, nil
		default:
			_ = conn.Close()
		}
	}
}

type limitedConn struct {
	net.Conn

	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()

	c.once.Do(c.release)

	return err
}
