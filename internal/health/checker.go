// Package health provides Tor connectivity verification beyond SOCKS5 handshakes.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// CheckURL is the Tor Project's official "am I using Tor" JSON endpoint.
var CheckURL = "https://check.torproject.org/api/ip"

// TorCheckResult represents the result of a Tor connectivity test.
type TorCheckResult struct {
	Port           int    `json:"port"`
	IsListening    bool   `json:"is_listening"`
	SOCKSHandshake bool   `json:"socks_handshake"`
	TorConnected   bool   `json:"tor_connected"`
	ExitIP         string `json:"exit_ip,omitempty"`
	IsTor          bool   `json:"is_tor"`
	Duration       string `json:"duration"`
	DurationMillis int64  `json:"duration_ms"`
	Error          string `json:"error,omitempty"`
}

// TorProjectCheckResponse matches the JSON from check.torproject.org/api/ip.
type TorProjectCheckResponse struct {
	IsTor bool   `json:"IsTor"`
	IP    string `json:"IP"`
}

// CheckPort performs a comprehensive connectivity test on 127.0.0.1:port.
func CheckPort(port int, timeout time.Duration) TorCheckResult {
	return CheckPortOn("127.0.0.1", port, timeout)
}

// CheckPortOn performs a comprehensive connectivity test on host:port:
// TCP connect, SOCKS5 greeting, then an HTTPS request through the proxy to
// check.torproject.org to confirm traffic exits via Tor.
func CheckPortOn(host string, port int, timeout time.Duration) TorCheckResult {
	result := TorCheckResult{Port: port}
	start := time.Now()
	finish := func() TorCheckResult {
		d := time.Since(start)
		result.Duration = d.Round(time.Millisecond).String()
		result.DurationMillis = d.Milliseconds()
		return result
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))

	conn, err := net.DialTimeout("tcp", addr, minDur(timeout, 5*time.Second))
	if err != nil {
		result.Error = fmt.Sprintf("port not listening: %v", err)
		return finish()
	}
	result.IsListening = true

	_ = conn.SetDeadline(time.Now().Add(minDur(timeout, 5*time.Second)))
	ok := false
	if _, err = conn.Write([]byte{0x05, 0x01, 0x00}); err == nil {
		resp := make([]byte, 2)
		if _, err = io.ReadFull(conn, resp); err == nil && resp[0] == 0x05 && resp[1] == 0x00 {
			ok = true
		}
	}
	conn.Close()
	if !ok {
		result.Error = "SOCKS5 handshake failed"
		return finish()
	}
	result.SOCKSHandshake = true

	torCheck, err := checkTorConnectivity(addr, timeout)
	if err != nil {
		result.Error = fmt.Sprintf("Tor connectivity check failed: %v", err)
		return finish()
	}

	result.TorConnected = torCheck.IsTor
	result.ExitIP = torCheck.IP
	result.IsTor = torCheck.IsTor
	if !torCheck.IsTor {
		result.Error = "traffic is NOT routed through Tor (check.torproject.org reports a non-Tor exit)"
	}
	return finish()
}

// CheckAllPorts tests multiple ports in parallel.
func CheckAllPorts(ports []int, timeout time.Duration, concurrency int) []TorCheckResult {
	if concurrency < 1 {
		concurrency = 1
	}
	results := make([]TorCheckResult, len(ports))
	sem := make(chan struct{}, concurrency)
	done := make(chan struct{}, len(ports))
	for i, port := range ports {
		go func(idx, p int) {
			sem <- struct{}{}
			defer func() { <-sem; done <- struct{}{} }()
			results[idx] = CheckPort(p, timeout)
		}(i, port)
	}
	for range ports {
		<-done
	}
	return results
}

// checkTorConnectivity uses the Tor Project's check API to verify Tor routing.
// DNS is resolved by Tor (the hostname is passed through SOCKS5), so the check
// itself does not leak DNS queries.
func checkTorConnectivity(proxyAddr string, timeout time.Duration) (*TorProjectCheckResponse, error) {
	proxyURL, err := url.Parse("socks5://" + proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}

	transport := &http.Transport{
		Proxy:               http.ProxyURL(proxyURL),
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout: timeout,
		DisableKeepAlives:   true,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: timeout}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CheckURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	// Mimic Tor Browser's generic UA rather than advertising Go.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; rv:128.0) Gecko/20100101 Firefox/128.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var checkResp TorProjectCheckResponse
	if err := json.Unmarshal(body, &checkResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &checkResp, nil
}

// QuickSOCKSCheck performs only a SOCKS5 handshake (no Tor verification).
func QuickSOCKSCheck(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return false
	}
	resp := make([]byte, 2)
	_, err = io.ReadFull(conn, resp)
	return err == nil && resp[0] == 0x05 && resp[1] == 0x00
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
