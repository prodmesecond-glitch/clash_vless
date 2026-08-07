// Command xhserver is a local test rig for proving a client transport both
// direct (T1) and hopped (T2). It runs, via the embedded engine, two servers:
// a Vision-reality HOP on 127.0.0.1:9001 and an xhttp-reality EXIT on
// 127.0.0.1:9002, each egressing freedom, and prints:
//
//	EXIT_URL=vless://…@127.0.0.1:9002?type=xhttp&security=reality&…   (use as a main)
//	ENTRY_OUTBOUND={…}                                                (the hop's xray outbound)
//
// Usage (from app/):
//
//	go run ./cmd/xhserver &                         # prints EXIT_URL= / ENTRY_OUTBOUND=
//	clashvless --config tmp/t main add '<EXIT_URL>'  # xhttp exit as the (non-Vision) main
//	# inject a sub node whose "outbound" is ENTRY_OUTBOUND, then:
//	clashvless --config tmp/t up hop                 # T2: exit dialed THROUGH the Vision hop
//	# curl -x socks5h://127.0.0.1:2084 https://cp.cloudflare.com/generate_204  → 204
//
// It is a separate main (pulls in proxy/vless/inbound) and is NOT part of the
// clashvless binary. Ports 9001/9002 are non-privileged on purpose (678 needs root).
package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"clashvless/internal/engine"
	"clashvless/internal/xray"

	_ "github.com/xtls/xray-core/proxy/vless/inbound"
)

func kp() (priv, pub string) {
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	return base64.RawURLEncoding.EncodeToString(k.Bytes()),
		base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
}

func reality(priv, sid string) map[string]any {
	return map[string]any{"dest": "www.cloudflare.com:443", "serverNames": []any{"www.cloudflare.com"},
		"privateKey": priv, "shortIds": []any{sid}}
}

func main() {
	privA, pubA := kp() // Vision-reality hop
	privB, pubB := kp() // xhttp-reality exit
	const (
		uuidA, sidA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "1111111111111111"
		uuidB, sidB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "2222222222222222"
		sni         = "www.cloudflare.com"
	)
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{"tag": "hop", "listen": "127.0.0.1", "port": 9001, "protocol": "vless",
				"settings":       map[string]any{"clients": []any{map[string]any{"id": uuidA, "flow": "xtls-rprx-vision"}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "tcp", "security": "reality", "realitySettings": reality(privA, sidA)}},
			map[string]any{"tag": "exit", "listen": "127.0.0.1", "port": 9002, "protocol": "vless",
				"settings": map[string]any{"clients": []any{map[string]any{"id": uuidB}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "xhttp", "security": "reality", "realitySettings": reality(privB, sidB),
					"xhttpSettings": map[string]any{"path": "/", "mode": "auto"}}},
		},
		"outbounds": []any{map[string]any{"protocol": "freedom", "tag": "direct"}},
	}
	j, _ := json.Marshal(cfg)
	inst, err := engine.Start(j)
	if err != nil {
		fmt.Fprintln(os.Stderr, "server start:", err)
		os.Exit(1)
	}
	defer inst.Close()

	entryURL := fmt.Sprintf("vless://%s@127.0.0.1:9001?encryption=none&flow=xtls-rprx-vision&type=tcp&security=reality&sni=%s&fp=chrome&pbk=%s&sid=%s#hop", uuidA, sni, pubA, sidA)
	exitURL := fmt.Sprintf("vless://%s@127.0.0.1:9002?encryption=none&type=xhttp&security=reality&sni=%s&fp=chrome&pbk=%s&sid=%s&path=/&mode=auto#exit", uuidB, sni, pubB, sidB)
	entryOB, err := xray.VlessToOutbound(entryURL, "proxy")
	if err != nil {
		fmt.Fprintln(os.Stderr, "entry outbound:", err)
		os.Exit(1)
	}
	fmt.Println("EXIT_URL=" + exitURL)
	fmt.Println("ENTRY_OUTBOUND=" + string(entryOB))
	fmt.Println("hop(:9001 vision-reality) + exit(:9002 xhttp-reality) up — Ctrl-C to stop")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
