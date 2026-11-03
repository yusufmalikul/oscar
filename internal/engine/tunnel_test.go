package engine

import (
	"net"
	"testing"
)

// A port this test process listens on is plain local, not a tunnel.
func TestPortForwardedLocalListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	if checkForwarded(port) {
		t.Fatalf("port %s held by the test process was seen as a tunnel", port)
	}
	if checkForwarded("1") {
		t.Fatalf("a port nobody listens on was seen as a tunnel")
	}
}
