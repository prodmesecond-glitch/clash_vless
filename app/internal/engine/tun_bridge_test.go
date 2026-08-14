package engine

import (
	"bytes"
	"strings"
	"testing"

	"clashvless/internal/xray"

	"github.com/xtls/xray-core/infra/conf/serial"
)

// The TUN bridge config (tun inbound → socks outbound) must be accepted by
// xray's own JSON parser — this catches settings-schema mistakes without needing
// root or actually creating a device (LoadJSONConfig only parses to protobuf).
func TestBridgeConfigParses(t *testing.T) {
	cfg, err := xray.BuildTunBridge(2084, "clashvless0", 1500, "warning")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serial.LoadJSONConfig(bytes.NewReader(cfg)); err != nil {
		t.Fatalf("xray rejected bridge config: %v\n%s", err, cfg)
	}
}

// With TUN mode active, BuildConfig decorates every config with SO_MARK on the
// dials + static hosts. It must still parse (dns app registered, sockopt schema
// right) and actually contain the decoration.
func TestTunDecoratedConfigParses(t *testing.T) {
	ob, err := xray.VlessToOutbound("vless://11111111-1111-1111-1111-111111111111@example.com:443?type=tcp&security=none", "main")
	if err != nil {
		t.Fatal(err)
	}
	xray.SetTunMode(0x1a2b, "eth0", map[string]string{"example.com": "1.2.3.4"})
	defer xray.SetTunMode(0, "", nil) // don't leak decoration into other tests
	live, err := xray.BuildConfig(2084, ob, nil, 0, "warning", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serial.LoadJSONConfig(bytes.NewReader(live)); err != nil {
		t.Fatalf("xray rejected TUN-decorated config: %v\n%s", err, live)
	}
	if s := string(live); !strings.Contains(s, `"mark"`) || !strings.Contains(s, `"interface"`) ||
		!strings.Contains(s, "eth0") || !strings.Contains(s, "1.2.3.4") {
		t.Fatalf("decoration (mark + bindToDevice + static host) missing:\n%s", s)
	}
}
