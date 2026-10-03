package torinstance

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// ControlConn is a minimal, authenticated Tor control-protocol client
// (https://spec.torproject.org/control-spec/). Only the subset needed by the
// manager is implemented: AUTHENTICATE, GETINFO and SIGNAL.
//
// A ControlConn is not safe for concurrent use.
type ControlConn struct {
	conn    net.Conn
	r       *bufio.Reader
	timeout time.Duration
}

// ControlError is returned when Tor replies with a non-2xx status code.
type ControlError struct {
	Code int
	Text string
}

func (e *ControlError) Error() string { return fmt.Sprintf("tor control error %d: %s", e.Code, e.Text) }

// DialControl connects to a Tor control port and authenticates using the
// cookie file (CookieAuthentication 1). If cookiePath is empty, null
// authentication is attempted.
func DialControl(addr, cookiePath string, timeout time.Duration) (*ControlConn, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("connect control port: %w", err)
	}
	c := &ControlConn{conn: conn, r: bufio.NewReader(conn), timeout: timeout}

	authCmd := "AUTHENTICATE"
	if cookiePath != "" {
		cookie, err := os.ReadFile(cookiePath)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read auth cookie: %w", err)
		}
		if len(cookie) != 32 {
			conn.Close()
			return nil, fmt.Errorf("auth cookie has unexpected length %d (want 32)", len(cookie))
		}
		authCmd += " " + hex.EncodeToString(cookie)
	}

	if _, err := c.Command(authCmd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	return c, nil
}

// Close sends QUIT (best effort) and closes the connection.
func (c *ControlConn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	_ = c.conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = c.conn.Write([]byte("QUIT\r\n"))
	return c.conn.Close()
}

// Command sends a single command line and returns the reply lines with their
// status prefix stripped. Multi-line data blocks ("250+key=") are joined with "\n".
func (c *ControlConn) Command(line string) ([]string, error) {
	if strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("control command must be a single line")
	}
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	return c.readReply()
}

func (c *ControlConn) readReply() ([]string, error) {
	var lines []string
	for {
		raw, err := c.r.ReadString('\n')
		if err != nil {
			return lines, fmt.Errorf("read: %w", err)
		}
		raw = strings.TrimRight(raw, "\r\n")
		if len(raw) < 4 {
			return lines, fmt.Errorf("malformed reply line %q", raw)
		}
		code, err := strconv.Atoi(raw[:3])
		if err != nil {
			return lines, fmt.Errorf("malformed status code in %q", raw)
		}
		sep, text := raw[3], raw[4:]

		switch sep {
		case '-': // mid-reply line
			lines = append(lines, text)
		case '+': // data reply: read until a line containing only "."
			var data []string
			for {
				dl, err := c.r.ReadString('\n')
				if err != nil {
					return lines, fmt.Errorf("read data: %w", err)
				}
				dl = strings.TrimRight(dl, "\r\n")
				if dl == "." {
					break
				}
				data = append(data, strings.TrimPrefix(dl, ".")) // dot-unstuffing
			}
			lines = append(lines, text+strings.Join(data, "\n"))
		case ' ': // end of reply
			if code < 200 || code > 299 {
				return lines, &ControlError{Code: code, Text: text}
			}
			if text != "OK" {
				lines = append(lines, text)
			}
			return lines, nil
		default:
			return lines, fmt.Errorf("malformed reply separator in %q", raw)
		}
	}
}

// GetInfo issues GETINFO for a single key and returns its value.
func (c *ControlConn) GetInfo(key string) (string, error) {
	lines, err := c.Command("GETINFO " + key)
	if err != nil {
		return "", err
	}
	prefix := key + "="
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimPrefix(l, prefix), nil
		}
	}
	return "", fmt.Errorf("GETINFO %s: key missing from reply", key)
}

// Signal sends SIGNAL <name> (e.g. NEWNYM, SHUTDOWN, HALT, RELOAD).
func (c *ControlConn) Signal(name string) error {
	_, err := c.Command("SIGNAL " + name)
	return err
}

// BootstrapStatus is the parsed form of GETINFO status/bootstrap-phase.
type BootstrapStatus struct {
	Progress int
	Tag      string
	Summary  string
	Warning  string
}

// GetBootstrap queries and parses the current bootstrap phase.
func (c *ControlConn) GetBootstrap() (BootstrapStatus, error) {
	v, err := c.GetInfo("status/bootstrap-phase")
	if err != nil {
		return BootstrapStatus{}, err
	}
	return ParseBootstrapPhase(v), nil
}

// ParseBootstrapPhase parses a value such as
//
//	NOTICE BOOTSTRAP PROGRESS=85 TAG=ap_conn_done SUMMARY="Connected to a relay to build circuits"
func ParseBootstrapPhase(v string) BootstrapStatus {
	var bs BootstrapStatus
	for k, val := range parseKeywordArgs(v) {
		switch k {
		case "PROGRESS":
			bs.Progress, _ = strconv.Atoi(val)
		case "TAG":
			bs.Tag = val
		case "SUMMARY":
			bs.Summary = val
		case "WARNING":
			bs.Warning = val
		}
	}
	return bs
}

// parseKeywordArgs extracts KEY=value and KEY="quoted value" pairs.
func parseKeywordArgs(s string) map[string]string {
	out := make(map[string]string)
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		start := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			continue // bare word (e.g. NOTICE, BOOTSTRAP)
		}
		key := s[start:i]
		i++ // skip '='
		if i < len(s) && s[i] == '"' {
			i++
			var sb strings.Builder
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				sb.WriteByte(s[i])
				i++
			}
			i++ // closing quote
			out[key] = sb.String()
		} else {
			vs := i
			for i < len(s) && s[i] != ' ' {
				i++
			}
			out[key] = s[vs:i]
		}
	}
	return out
}
