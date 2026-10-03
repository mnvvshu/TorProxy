// Command faketor is a lightweight stand-in for tor.exe used by the test suite.
//
// It understands just enough of Tor to exercise TorProxyManager end to end:
//
//   - parses "-f <torrc>" for SocksPort, ControlPort, DataDirectory and Log
//   - writes a 32-byte control_auth_cookie into DataDirectory
//   - answers the SOCKS5 greeting on SocksPort
//   - implements AUTHENTICATE, GETINFO status/bootstrap-phase, GETINFO version,
//     SIGNAL NEWNYM / SHUTDOWN / HALT on ControlPort
//
// Behaviour can be tuned with environment variables:
//
//	FAKETOR_BOOTSTRAP_MS   time to reach 100% bootstrap (default 300)
//	FAKETOR_FAIL=1         log an [err] line and exit(1) immediately
//	FAKETOR_IGNORE_SHUTDOWN=1  ignore SIGNAL SHUTDOWN (forces a kill)
//
// Creating a file named "crash_now" in the DataDirectory makes the process
// exit with status 1 within ~100ms (simulates a crash).
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	logFile    *os.File
	startTime  = time.Now()
	bootstrapD = 300 * time.Millisecond
	newnyms    atomic.Int32
)

func logf(level, format string, args ...interface{}) {
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format("Jan 02 15:04:05.000"), level, fmt.Sprintf(format, args...))
	os.Stdout.WriteString(line)
	if logFile != nil {
		logFile.WriteString(line)
	}
}

func main() {
	var torrcPath string
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-f" && i+1 < len(os.Args) {
			torrcPath = os.Args[i+1]
			i++
		}
	}
	if torrcPath == "" {
		fmt.Println("[err] no torrc given")
		os.Exit(2)
	}
	if v, err := strconv.Atoi(os.Getenv("FAKETOR_BOOTSTRAP_MS")); err == nil {
		bootstrapD = time.Duration(v) * time.Millisecond
	}

	conf, err := parseTorrc(torrcPath)
	if err != nil {
		fmt.Printf("[err] reading torrc: %v\n", err)
		os.Exit(2)
	}

	if lf := conf["log"]; lf != "" {
		// "notice file <path>"
		parts := strings.SplitN(lf, " ", 3)
		if len(parts) == 3 && parts[1] == "file" {
			logFile, _ = os.OpenFile(unquote(parts[2]), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		}
	}

	if os.Getenv("FAKETOR_FAIL") == "1" {
		logf("err", "Simulated fatal startup error")
		os.Exit(1)
	}

	dataDir := unquote(conf["datadirectory"])
	cookie := make([]byte, 32)
	rand.Read(cookie)

	socksAddrs := socksPortsFromTorrc(torrcPath)
	ctrlAddr := conf["controlport"]

	var socksLns []net.Listener
	for _, socksAddr := range socksAddrs {
		ln, err := net.Listen("tcp", socksAddr)
		if err != nil {
			logf("warn", "Could not bind to %s: %v", socksAddr, err)
			logf("err", "Failed to bind one of the listener ports.")
			os.Exit(1)
		}
		socksLns = append(socksLns, ln)
		logf("notice", "Opened Socks listener on %s", socksAddr)
	}
	ctrlLn, err := net.Listen("tcp", ctrlAddr)
	if err != nil {
		logf("warn", "Could not bind to %s: %v", ctrlAddr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "control_auth_cookie"), cookie, 0o600); err != nil {
		logf("err", "cannot write cookie: %v", err)
		os.Exit(1)
	}
	logf("notice", "Opened Control listener on %s", ctrlAddr)

	go watchCrashFile(dataDir)
	for _, ln := range socksLns {
		go serveSocks(ln)
	}
	serveControl(ctrlLn, cookie)
}

func watchCrashFile(dataDir string) {
	marker := filepath.Join(dataDir, "crash_now")
	for {
		if _, err := os.Stat(marker); err == nil {
			os.Remove(marker)
			logf("err", "Simulated crash")
			os.Exit(1)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func progress() int {
	if bootstrapD <= 0 {
		return 100
	}
	p := int(float64(time.Since(startTime)) / float64(bootstrapD) * 100)
	if p > 100 {
		p = 100
	}
	return p
}

func serveSocks(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			hdr := make([]byte, 2)
			if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
				return
			}
			methods := make([]byte, hdr[1])
			io.ReadFull(c, methods)
			c.Write([]byte{5, 0})
			// Reject any CONNECT with "general failure" — no real network.
			req := make([]byte, 4)
			if _, err := io.ReadFull(c, req); err == nil {
				c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
			}
		}(c)
	}
}

func serveControl(ln net.Listener, cookie []byte) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handleControl(c, cookie)
	}
}

func handleControl(c net.Conn, cookie []byte) {
	defer c.Close()
	r := bufio.NewReader(c)
	authed := false
	write := func(s string) { c.Write([]byte(s + "\r\n")) }

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		cmd := strings.ToUpper(fields[0])

		if !authed && cmd != "AUTHENTICATE" && cmd != "PROTOCOLINFO" && cmd != "QUIT" {
			write("514 Authentication required.")
			return
		}

		switch cmd {
		case "AUTHENTICATE":
			if len(fields) < 2 || !strings.EqualFold(fields[1], hex.EncodeToString(cookie)) {
				write("515 Authentication failed: Wrong length on authentication cookie.")
				return
			}
			authed = true
			write("250 OK")
		case "GETINFO":
			if len(fields) < 2 {
				write("552 Missing key")
				continue
			}
			switch fields[1] {
			case "status/bootstrap-phase":
				p := progress()
				summary := "Loading relay descriptors"
				tag := "loading_descriptors"
				if p >= 100 {
					summary, tag = "Done", "done"
				}
				write(fmt.Sprintf("250-status/bootstrap-phase=NOTICE BOOTSTRAP PROGRESS=%d TAG=%s SUMMARY=\"%s\"", p, tag, summary))
				write("250 OK")
			case "version":
				write("250-version=0.4.8.99 (faketor)")
				write("250 OK")
			case "config-text":
				write("250+config-text=")
				write("SocksPort 127.0.0.1:0")
				write(".")
				write("250 OK")
			default:
				write(fmt.Sprintf("552 Unrecognized key \"%s\"", fields[1]))
			}
		case "SIGNAL":
			if len(fields) < 2 {
				write("552 Missing signal")
				continue
			}
			switch strings.ToUpper(fields[1]) {
			case "NEWNYM":
				newnyms.Add(1)
				write("250 OK")
			case "SHUTDOWN", "HALT":
				write("250 OK")
				if os.Getenv("FAKETOR_IGNORE_SHUTDOWN") != "1" {
					logf("notice", "Received SHUTDOWN signal; exiting")
					time.Sleep(20 * time.Millisecond)
					os.Exit(0)
				}
			default:
				write("552 Unrecognized signal code")
			}
		case "QUIT":
			write("250 closing connection")
			return
		default:
			write(fmt.Sprintf("510 Unrecognized command \"%s\"", fields[0]))
		}
	}
}

func socksPortsFromTorrc(path string) []string {
	data, _ := os.ReadFile(path)
	var out []string
	for _, raw := range strings.Split(string(data), "\n") {
		parts := strings.Fields(strings.TrimSpace(raw))
		if len(parts) == 2 && strings.EqualFold(parts[0], "SocksPort") {
			out = append(out, parts[1])
		}
	}
	return out
}

func parseTorrc(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, raw := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		parts := strings.SplitN(l, " ", 2)
		if len(parts) != 2 {
			continue
		}
		out[strings.ToLower(parts[0])] = strings.TrimSpace(parts[1])
	}
	for _, k := range []string{"socksport", "controlport", "datadirectory"} {
		if out[k] == "" {
			return nil, fmt.Errorf("missing %s", k)
		}
	}
	return out, nil
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`)
	}
	return s
}
