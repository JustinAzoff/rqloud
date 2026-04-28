package rqloud

import (
	"context"
	"fmt"
	"net"
	"time"

	"tailscale.com/tsnet"
)

// addr is a simple net.Addr implementation for an advertised hostname:port.
type addr struct {
	host string
	port int
}

func NewAddr(host string, port int) net.Addr {
	return &addr{host: host, port: port}
}

func (a *addr) Network() string { return "tcp" }
func (a *addr) String() string  { return net.JoinHostPort(a.host, fmt.Sprintf("%d", a.port)) }

// tsnetDialer dials over tsnet and writes a mux header byte, compatible with
// rqlite's tcp.Mux protocol on the remote end.
type tsnetDialer struct {
	srv    *tsnet.Server
	header byte
}

func (d *tsnetDialer) Dial(address string, timeout time.Duration) (conn net.Conn, retErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := d.srv.Dial(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("tsnet dial %s: %w", address, err)
	}
	defer func() {
		if retErr != nil && conn != nil {
			conn.Close()
		}
	}()

	// Write the mux header byte so the remote tcp.Mux routes this connection.
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set write deadline: %w", err)
	}
	if _, err := conn.Write([]byte{d.header}); err != nil {
		return nil, fmt.Errorf("write mux header: %w", err)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear write deadline: %w", err)
	}
	return conn, nil
}

// tsnetRaftLayer implements store.Layer using a mux sub-listener for Accept
// and a tsnetDialer for Dial.
type tsnetRaftLayer struct {
	ln     net.Listener // mux sub-listener for Raft traffic
	addr   net.Addr     // advertised address (hostname:port)
	dialer *tsnetDialer
}

func (l *tsnetRaftLayer) Accept() (net.Conn, error) { return l.ln.Accept() }
func (l *tsnetRaftLayer) Close() error              { return l.ln.Close() }
func (l *tsnetRaftLayer) Addr() net.Addr            { return l.addr }

func (l *tsnetRaftLayer) Dial(address string, timeout time.Duration) (net.Conn, error) {
	return l.dialer.Dial(address, timeout)
}
