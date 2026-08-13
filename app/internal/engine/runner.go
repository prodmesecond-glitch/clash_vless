// Package engine runs xray-core in-process (embedded, so the whole thing
// compiles to a single binary) and will host the failover supervisor.
package engine

import (
	"bytes"
	"fmt"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"

	// Register ONLY the xray features this app uses (instead of
	// main/distro/all) to keep the binary small. If a config ever uses a
	// protocol/transport not listed here, core.New will fail at runtime.
	_ "github.com/xtls/xray-core/app/dispatcher"
	_ "github.com/xtls/xray-core/app/dns" // static hosts for the exit's own domain in TUN mode
	_ "github.com/xtls/xray-core/app/log"
	_ "github.com/xtls/xray-core/app/proxyman/inbound"
	_ "github.com/xtls/xray-core/app/proxyman/outbound"
	_ "github.com/xtls/xray-core/app/router"

	_ "github.com/xtls/xray-core/proxy/blackhole"      // "block" outbound
	_ "github.com/xtls/xray-core/proxy/freedom"        // "direct" outbound
	_ "github.com/xtls/xray-core/proxy/socks"          // local inbound + bridge's socks outbound
	_ "github.com/xtls/xray-core/proxy/tun"            // TUN inbound (system-wide capture, bridge instance)
	_ "github.com/xtls/xray-core/proxy/vless/outbound" // main + entry nodes

	_ "github.com/xtls/xray-core/transport/internet/grpc"
	_ "github.com/xtls/xray-core/transport/internet/reality"
	_ "github.com/xtls/xray-core/transport/internet/splithttp" // xhttp (type=xhttp)
	_ "github.com/xtls/xray-core/transport/internet/tcp"
	_ "github.com/xtls/xray-core/transport/internet/tls"
	_ "github.com/xtls/xray-core/transport/internet/websocket"
)

// Instance is a running, in-process xray core. Close() stops it.
type Instance = core.Instance

// Start builds an xray instance from a JSON config and starts it in-process.
// Close the returned instance to stop serving.
func Start(jsonConfig []byte) (*core.Instance, error) {
	cfg, err := serial.LoadJSONConfig(bytes.NewReader(jsonConfig))
	if err != nil {
		return nil, fmt.Errorf("load xray json: %w", err)
	}
	inst, err := core.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("new xray instance: %w", err)
	}
	if err := inst.Start(); err != nil {
		_ = inst.Close()
		return nil, fmt.Errorf("start xray: %w", err)
	}
	return inst, nil
}
