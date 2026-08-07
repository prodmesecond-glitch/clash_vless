package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/proxy"
)

// egressURL is a tiny 204 endpoint used to prove a chain actually reaches the
// internet (not just that a TCP hop is up). HTTPS on 443 (not plain HTTP:80,
// which some exits filter) so a passing ping matches what real traffic can do.
const egressURL = "https://cp.cloudflare.com/generate_204"

// speedURL is a large download used for throughput tests (time- and byte-bounded).
const speedURL = "https://speed.cloudflare.com/__down?bytes=52428800"

// SpeedThroughSOCKS downloads through the SOCKS port for up to budget and returns
// throughput in Mbps. It's a one-shot, time- and byte-bounded speedtest.
func SpeedThroughSOCKS(ctx context.Context, socksPort int, budget time.Duration) (float64, bool) {
	d, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), nil, &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		return 0, false
	}
	tr := &http.Transport{
		DialContext:       func(_ context.Context, network, addr string) (net.Conn, error) { return d.Dial(network, addr) },
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: tr}
	dl, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	req, err := http.NewRequestWithContext(dl, http.MethodGet, speedURL, nil)
	if err != nil {
		return 0, false
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, resp.Body) // reads until the budget deadline cancels it
	secs := time.Since(start).Seconds()
	if n < 1<<16 || secs <= 0 {
		return 0, false
	}
	return float64(n*8) / secs / 1e6, true
}

// TCPLatency measures how long a raw TCP connect to host:port takes. It's the
// cheap pre-rank used to try the fastest entries first, before the (costlier)
// end-to-end egress test.
func TCPLatency(host string, port int, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	d := net.Dialer{Timeout: timeout}
	c, err := d.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return 0, err
	}
	_ = c.Close()
	return time.Since(start), nil
}

// freePort asks the OS for an unused loopback TCP port. Throwaway probe instances
// use a fresh one each time, so a service holding any fixed port can't wedge probing.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitPort blocks until a local TCP port accepts connections, up to timeout.
func waitPort(port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// EgressThroughSOCKS makes a 204 request through the given local SOCKS5 port and
// returns its round-trip latency, or an error if the chain can't reach the net.
func EgressThroughSOCKS(ctx context.Context, socksPort int, timeout time.Duration) (time.Duration, error) {
	d, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socksPort), nil, &net.Dialer{Timeout: timeout})
	if err != nil {
		return 0, err
	}
	tr := &http.Transport{
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			return d.Dial(network, addr)
		},
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, egressURL, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("egress status %d", resp.StatusCode)
	}
	return time.Since(start), nil
}
