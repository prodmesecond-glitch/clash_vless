package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/proxy"
)

// egressURL is a tiny 204 endpoint used to prove a chain actually reaches the
// internet (not just that a TCP hop is up).
const egressURL = "http://www.gstatic.com/generate_204"

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
