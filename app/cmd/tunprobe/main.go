// Command tunprobe is a root/Administrator-only lab check: it starts JUST the
// TUN bridge (xray's native tun inbound → local SOCKS), confirms the OS network
// device gets created, then tears it down. It deliberately does NOT change
// routing or DNS, so it can never disrupt host connectivity — it only answers
// "does xray's native TUN inbound create a device in this build, on this kernel?".
//
//	sudo go -C app run ./cmd/tunprobe        # Linux
//	go run ./cmd/tunprobe                     # Windows (elevated; needs wintun.dll)
//
// Separate main (pulls in the tun inbound); not part of the app binary.
package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"clashvless/internal/engine"
	"clashvless/internal/tun"
	"clashvless/internal/xray"
)

func main() {
	const name = "cvprobe0"
	if !tun.Supported() {
		fmt.Println("tunprobe: TUN is not supported on this OS")
		os.Exit(1)
	}
	if !tun.Privileged() {
		fmt.Println("tunprobe: needs root/Administrator to create a TUN device")
		os.Exit(1)
	}

	cfg, err := xray.BuildTunBridge(2084, name, 1500, "warning")
	if err != nil {
		fmt.Println("build bridge config:", err)
		os.Exit(1)
	}
	fmt.Printf("starting bridge (tun %q → socks 127.0.0.1:2084)…\n", name)
	inst, err := engine.Start(cfg)
	if err != nil {
		fmt.Println("✗ bridge failed to start:", err)
		fmt.Println("  (Windows needs wintun.dll next to the binary)")
		os.Exit(1)
	}

	ok := false
	for i := 0; i < 40; i++ { // race the async device creation, ~6s
		if ifi, err := net.InterfaceByName(name); err == nil {
			fmt.Printf("✓ device created: %s (index %d, mtu %d, flags %v)\n", ifi.Name, ifi.Index, ifi.MTU, ifi.Flags)
			ok = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !ok {
		fmt.Println("✗ device was not created within 6s")
	}

	_ = inst.Close()
	time.Sleep(500 * time.Millisecond)
	if _, err := net.InterfaceByName(name); err != nil {
		fmt.Println("✓ device removed after close")
	} else {
		fmt.Println("… device still present after close (harmless — no routes were ever set)")
	}

	if !ok {
		os.Exit(1)
	}
	fmt.Println("tunprobe OK — xray's native TUN inbound works here. Routing/DNS were NOT touched.")
}
