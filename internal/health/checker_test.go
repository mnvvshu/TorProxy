package health

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// startSOCKS5Proxy runs a minimal SOCKS5 CONNECT proxy for tests.
func startSOCKS5Proxy(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSOCKS(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func handleSOCKS(c net.Conn) {
	defer c.Close()
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return
	}
	io.ReadFull(c, make([]byte, hdr[1]))
	c.Write([]byte{5, 0})

	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 1:
		ip := make([]byte, 4)
		io.ReadFull(c, ip)
		host = net.IP(ip).String()
	case 3:
		l := make([]byte, 1)
		io.ReadFull(c, l)
		name := make([]byte, l[0])
		io.ReadFull(c, name)
		host = string(name)
	default:
		return
	}
	pb := make([]byte, 2)
	io.ReadFull(c, pb)
	port := binary.BigEndian.Uint16(pb)

	up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go io.Copy(up, c)
	io.Copy(c, up)
}

func withCheckServer(t *testing.T, body string, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	old := CheckURL
	CheckURL = srv.URL + "/api/ip"
	t.Cleanup(func() { CheckURL = old })
}

func TestCheckPort_TorDetected(t *testing.T) {
	withCheckServer(t, `{"IsTor":true,"IP":"185.220.101.1"}`, 200)
	port := startSOCKS5Proxy(t)
	r := CheckPort(port, 5*time.Second)
	if !r.IsListening || !r.SOCKSHandshake || !r.IsTor || r.ExitIP != "185.220.101.1" || r.Error != "" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestCheckPort_NotTor(t *testing.T) {
	withCheckServer(t, `{"IsTor":false,"IP":"1.2.3.4"}`, 200)
	port := startSOCKS5Proxy(t)
	r := CheckPort(port, 5*time.Second)
	if r.IsTor || r.Error == "" {
		t.Fatalf("expected non-Tor error, got %+v", r)
	}
}

func TestCheckPort_HTTPError(t *testing.T) {
	withCheckServer(t, `oops`, 503)
	port := startSOCKS5Proxy(t)
	r := CheckPort(port, 5*time.Second)
	if r.TorConnected || r.Error == "" {
		t.Fatalf("expected HTTP error, got %+v", r)
	}
}

func TestCheckPort_NotListening(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	r := CheckPort(port, time.Second)
	if r.IsListening || r.Error == "" {
		t.Fatalf("expected not-listening error, got %+v", r)
	}
}

func TestCheckPort_NotSOCKS(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Write([]byte("HTTP/1.0 400 Bad Request\r\n\r\n"))
			c.Close()
		}
	}()
	r := CheckPort(ln.Addr().(*net.TCPAddr).Port, time.Second)
	if !r.IsListening || r.SOCKSHandshake {
		t.Fatalf("expected SOCKS handshake failure, got %+v", r)
	}
}

func TestCheckAllPorts(t *testing.T) {
	withCheckServer(t, `{"IsTor":true,"IP":"10.0.0.1"}`, 200)
	ports := []int{startSOCKS5Proxy(t), startSOCKS5Proxy(t), startSOCKS5Proxy(t)}
	res := CheckAllPorts(ports, 5*time.Second, 2)
	for i, r := range res {
		if r.Port != ports[i] || !r.IsTor {
			t.Errorf("result %d: %+v", i, r)
		}
	}
	if !QuickSOCKSCheck(ports[0]) {
		t.Error("QuickSOCKSCheck failed against working proxy")
	}
}
