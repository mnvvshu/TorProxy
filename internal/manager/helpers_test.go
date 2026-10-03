package manager

import (
	"net"
	"strconv"
	"testing"
)

func listen(t *testing.T, port int) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("listen %d: %v", port, err)
	}
	return ln
}
