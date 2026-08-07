// Command xhprobe is a local LAB that mimics the real failover topology so we can
// map, with hard data, exactly which chains carry traffic. All servers run
// in-process and egress freedom to the REAL internet:
//
//	entry-vision  :9001  vless / tcp   / reality + xtls-rprx-vision
//	entry-plain   :9003  vless / tcp   / reality (no flow)
//	exit-xhttp    :9002  vless / xhttp / reality
//	exit-tcp      :9004  vless / tcp   / reality
//	exit-plain    :9006  vless / tcp   / security=none
//
// It then dials every EXIT through every HOP (direct / vision / plain) with both
// chaining mechanisms (sockopt.dialerProxy vs outbound proxySettings) and reports
// the 204 result plus the TLS cert CN a real https://www.google.com handshake
// yields through the chain:
//
//	OK(www.google.com)  chain carries traffic, correct cert
//	WRONGCERT(host)     the named reality served its real dest → it rejected the relay
//	FAIL(...)           chain dead (stall / EOF)
//
// Separate main (pulls in vless/inbound); NOT part of the app binary.
package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"clashvless/internal/engine"
	"clashvless/internal/xray"

	"golang.org/x/net/proxy"

	_ "github.com/xtls/xray-core/proxy/vless/inbound"
)

func kp() (priv, pub string) {
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	return base64.RawURLEncoding.EncodeToString(k.Bytes()), base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
}

func reality(priv, sid, dest string) map[string]any {
	return map[string]any{"dest": dest + ":443", "serverNames": []any{dest}, "privateKey": priv, "shortIds": []any{sid}}
}

func mustOB(uri string) json.RawMessage {
	ob, err := xray.VlessToOutbound(uri, "x")
	if err != nil {
		panic(err)
	}
	return ob
}
func asMap(ob json.RawMessage) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(ob, &m)
	return m
}

// clientCfg builds a socks-in + outbounds client config. entryURL=="" → direct.
// mech "dialer" → streamSettings.sockopt.dialerProxy; "proxy" → outbound proxySettings.
func clientCfg(port int, exitURL, entryURL, mech string) []byte {
	main := asMap(mustOB(exitURL))
	main["tag"] = "main"
	outs := []any{main}
	if entryURL != "" {
		entry := asMap(mustOB(entryURL))
		entry["tag"] = "entry"
		if mech == "proxy" {
			main["proxySettings"] = map[string]any{"tag": "entry"}
		} else {
			ss, _ := main["streamSettings"].(map[string]any)
			if ss == nil {
				ss = map[string]any{}
				main["streamSettings"] = ss
			}
			ss["sockopt"] = map[string]any{"dialerProxy": "entry"}
		}
		outs = append(outs, entry)
	}
	outs = append(outs, map[string]any{"protocol": "freedom", "tag": "direct"})
	cfg := map[string]any{
		"log":       map[string]any{"loglevel": "error"},
		"inbounds":  []any{map[string]any{"tag": "in", "listen": "127.0.0.1", "port": port, "protocol": "socks", "settings": map[string]any{"udp": true}}},
		"outbounds": outs,
	}
	j, _ := json.Marshal(cfg)
	return j
}

func probe204(port int) string {
	d, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", port), nil, &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		return "dialer-err"
	}
	c := &http.Client{Transport: &http.Transport{
		DialContext:       func(_ context.Context, n, a string) (net.Conn, error) { return d.Dial(n, a) },
		DisableKeepAlives: true,
	}, Timeout: 10 * time.Second}
	resp, err := c.Get("https://cp.cloudflare.com/generate_204")
	if err != nil {
		return "FAIL"
	}
	resp.Body.Close()
	return fmt.Sprintf("%d", resp.StatusCode)
}

// browseCert opens a real TLS handshake to google THROUGH the chain and returns
// the leaf cert CN (InsecureSkipVerify so a reality-fallback wrong cert is visible).
func browseCert(port int) string {
	d, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", port), nil, &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		return "dialer-err"
	}
	raw, err := d.Dial("tcp", "www.google.com:443")
	if err != nil {
		return "FAIL(dial)"
	}
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	tc := tls.Client(raw, &tls.Config{ServerName: "www.google.com", InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		tc.Close()
		return "FAIL(tls)"
	}
	defer tc.Close()
	cs := tc.ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		return "no-cert"
	}
	cn := cs.PeerCertificates[0].Subject.CommonName
	if cn == "www.google.com" || cn == "*.google.com" {
		return "OK"
	}
	return "WRONGCERT(" + cn + ")"
}

func main() {
	privV, pubV := kp()
	privP, pubP := kp()
	privX, pubX := kp()
	privT, pubT := kp()
	const (
		idV, sV  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "1111111111111111"
		idP, sP  = "cccccccc-cccc-cccc-cccc-cccccccccccc", "3333333333333333"
		idX, sX  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "2222222222222222"
		idT, sT  = "dddddddd-dddd-dddd-dddd-dddddddddddd", "4444444444444444"
		idPl     = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
		dV, dP   = "www.microsoft.com", "www.apple.com"
		dX, dT   = "www.amazon.com", "www.cloudflare.com"
	)
	server := map[string]any{
		"log": map[string]any{"loglevel": "error"},
		"inbounds": []any{
			map[string]any{"tag": "ev", "listen": "127.0.0.1", "port": 9001, "protocol": "vless",
				"settings":       map[string]any{"clients": []any{map[string]any{"id": idV, "flow": "xtls-rprx-vision"}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "tcp", "security": "reality", "realitySettings": reality(privV, sV, dV)}},
			map[string]any{"tag": "ep", "listen": "127.0.0.1", "port": 9003, "protocol": "vless",
				"settings":       map[string]any{"clients": []any{map[string]any{"id": idP}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "tcp", "security": "reality", "realitySettings": reality(privP, sP, dP)}},
			map[string]any{"tag": "xh", "listen": "127.0.0.1", "port": 9002, "protocol": "vless",
				"settings":       map[string]any{"clients": []any{map[string]any{"id": idX}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "xhttp", "security": "reality", "realitySettings": reality(privX, sX, dX), "xhttpSettings": map[string]any{"path": "/", "mode": "auto"}}},
			map[string]any{"tag": "tc", "listen": "127.0.0.1", "port": 9004, "protocol": "vless",
				"settings":       map[string]any{"clients": []any{map[string]any{"id": idT}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "tcp", "security": "reality", "realitySettings": reality(privT, sT, dT)}},
			map[string]any{"tag": "pl", "listen": "127.0.0.1", "port": 9006, "protocol": "vless",
				"settings":       map[string]any{"clients": []any{map[string]any{"id": idPl}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "tcp", "security": "none"}},
		},
		"outbounds": []any{map[string]any{"protocol": "freedom", "tag": "direct"}},
	}
	sj, _ := json.Marshal(server)
	si, err := engine.Start(sj)
	if err != nil {
		fmt.Fprintln(os.Stderr, "server start:", err)
		return
	}
	defer si.Close()
	time.Sleep(1500 * time.Millisecond)

	entV := fmt.Sprintf("vless://%s@127.0.0.1:9001?encryption=none&flow=xtls-rprx-vision&type=tcp&security=reality&sni=%s&fp=chrome&pbk=%s&sid=%s#hop-vision", idV, dV, pubV, sV)
	entP := fmt.Sprintf("vless://%s@127.0.0.1:9003?encryption=none&type=tcp&security=reality&sni=%s&fp=chrome&pbk=%s&sid=%s#hop-plain", idP, dP, pubP, sP)
	xhURL := fmt.Sprintf("vless://%s@127.0.0.1:9002?encryption=none&type=xhttp&security=reality&sni=%s&fp=chrome&pbk=%s&sid=%s&path=/&mode=auto#exit-xhttp", idX, dX, pubX, sX)
	tcURL := fmt.Sprintf("vless://%s@127.0.0.1:9004?encryption=none&type=tcp&security=reality&sni=%s&fp=chrome&pbk=%s&sid=%s#exit-tcp", idT, dT, pubT, sT)
	plURL := fmt.Sprintf("vless://%s@127.0.0.1:9006?encryption=none&type=tcp&security=none#exit-plain", idPl)

	exits := []struct{ name, url string }{
		{"exit=plain (none) ", plURL},
		{"exit=xhttp (reality)", xhURL},
		{"exit=tcp   (reality)", tcURL},
	}
	hops := []struct{ name, url string }{
		{"direct", ""},
		{"VISION-hop", entV},
		{"plain-hop ", entP},
	}
	mechs := []string{"dialer", "proxy"}

	fmt.Println("=== connection matrix (exit dialed through hop; egress → real internet) ===")
	fmt.Printf("%-20s  %-11s  %-7s  %-5s  %s\n", "exit", "hop", "mech", "204", "google cert")
	port := 2200
	for _, ex := range exits {
		for _, hp := range hops {
			run := func(mech string) {
				port++
				inst, err := engine.Start(clientCfg(port, ex.url, hp.url, mech))
				if err != nil {
					fmt.Printf("%-20s  %-11s  %-7s  start-err %v\n", ex.name, hp.name, mech, err)
					return
				}
				time.Sleep(1100 * time.Millisecond)
				fmt.Printf("%-20s  %-11s  %-7s  %-5s  %s\n", ex.name, hp.name, mech, probe204(port), browseCert(port))
				inst.Close()
				time.Sleep(150 * time.Millisecond)
			}
			if hp.url == "" {
				run("-") // direct: no chaining mechanism
			} else {
				for _, m := range mechs {
					run(m)
				}
			}
		}
	}
	fmt.Println("\nreality dests (a WRONGCERT of one = that reality rejected the relay):")
	fmt.Printf("  VISION-hop=%s  plain-hop=%s  xhttp-exit=%s  tcp-exit=%s\n", dV, dP, dX, dT)
	fmt.Println("\n--- servers stay up for manual experiments (Ctrl-C to stop) ---")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
