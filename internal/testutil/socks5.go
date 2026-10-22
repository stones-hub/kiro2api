package testutil

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
)

// SOCKS5Server is a minimal RFC 1928/1929 CONNECT server for tests.
type SOCKS5Server struct {
	Addr     string
	User     string
	Pass     string
	Connects atomic.Int64
	ln       net.Listener
}

// NewSOCKS5Server listens on loopback; empty user disables authentication.
func NewSOCKS5Server(t testing.TB, user, pass string) *SOCKS5Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &SOCKS5Server{Addr: ln.Addr().String(), User: user, Pass: pass, ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

// Close stops accepting connections, so later dials to Addr are refused.
func (s *SOCKS5Server) Close() { _ = s.ln.Close() }

// Port returns the listening port.
func (s *SOCKS5Server) Port() int {
	_, p, _ := net.SplitHostPort(s.Addr)
	n, _ := strconv.Atoi(p)
	return n
}

func (s *SOCKS5Server) serve(c net.Conn) {
	defer c.Close()
	b := make([]byte, 262)
	if _, err := io.ReadFull(c, b[:2]); err != nil || b[0] != 5 {
		return
	}
	methods := make([]byte, b[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	want := byte(0)
	if s.User != "" {
		want = 2
	}
	offered := false
	for _, m := range methods {
		offered = offered || m == want
	}
	if !offered {
		_, _ = c.Write([]byte{5, 0xff})
		return
	}
	_, _ = c.Write([]byte{5, want})
	if want == 2 {
		read := func() string {
			if _, err := io.ReadFull(c, b[:1]); err != nil {
				return ""
			}
			v := make([]byte, b[0])
			_, _ = io.ReadFull(c, v)
			return string(v)
		}
		if _, err := io.ReadFull(c, b[:1]); err != nil {
			return
		}
		user, pass := read(), read()
		if user != s.User || pass != s.Pass {
			_, _ = c.Write([]byte{1, 1})
			return
		}
		_, _ = c.Write([]byte{1, 0})
	}
	if _, err := io.ReadFull(c, b[:4]); err != nil || b[1] != 1 {
		return
	}
	var host string
	switch b[3] {
	case 1:
		ip := make([]byte, 4)
		_, _ = io.ReadFull(c, ip)
		host = net.IP(ip).String()
	case 3:
		_, _ = io.ReadFull(c, b[:1])
		name := make([]byte, b[0])
		_, _ = io.ReadFull(c, name)
		host = string(name)
	case 4:
		ip := make([]byte, 16)
		_, _ = io.ReadFull(c, ip)
		host = net.IP(ip).String()
	default:
		return
	}
	if _, err := io.ReadFull(c, b[:2]); err != nil {
		return
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(b[:2])))))
	if err != nil {
		_, _ = c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0}) // host unreachable
		return
	}
	defer target.Close()
	s.Connects.Add(1)
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go func() { _, _ = io.Copy(target, c); _ = target.Close() }()
	_, _ = io.Copy(c, target)
}

// ClosedPort returns a loopback port with nothing listening on it.
func ClosedPort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}
