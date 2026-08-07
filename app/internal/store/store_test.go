package store

import "testing"

func TestIsPlain(t *testing.T) {
	cases := []struct {
		url   string
		plain bool
	}{
		{"vless://x@45.1.1.1:444?type=tcp&security=none", true},
		{"vless://x@45.1.1.1:444?type=tcp", true}, // no security param → none
		{"vless://x@1.1.1.1:443?type=xhttp&security=reality&pbk=k", false},
		{"vless://x@1.1.1.1:443?type=tcp&security=reality&flow=xtls-rprx-vision", false},
		{"vless://x@1.1.1.1:443?type=tcp&security=tls", false},
		{"vless://x@1.1.1.1:443?flow=xtls-rprx-vision", false},
	}
	for _, c := range cases {
		if got := IsPlain(c.url); got != c.plain {
			t.Errorf("IsPlain(%q) = %v, want %v", c.url, got, c.plain)
		}
	}
}

func TestProtoTag(t *testing.T) {
	url := map[string]string{
		"vless://a@h:444?type=tcp&security=none":                        "plain",
		"vless://a@h:443?type=xhttp&security=reality":                   "reality·xhttp",
		"vless://a@h:443?type=tcp&security=reality&flow=xtls-rprx-vision": "vision",
		"vless://a@h:443?type=grpc&security=reality":                    "reality·grpc",
		"vless://a@h:443?type=ws&security=tls":                          "tls·ws",
	}
	for u, want := range url {
		if got := ProtoTag(u); got != want {
			t.Errorf("ProtoTag(%q) = %q, want %q", u, got, want)
		}
	}
	ob := map[string]string{
		`{"settings":{"vnext":[{"users":[{"flow":"xtls-rprx-vision"}]}]},"streamSettings":{"network":"tcp","security":"reality"}}`: "vision",
		`{"settings":{"vnext":[{"users":[{}]}]},"streamSettings":{"network":"grpc","security":"reality"}}`:                         "reality·grpc",
		`{"settings":{"vnext":[{"users":[{}]}]},"streamSettings":{"network":"ws","security":"tls"}}`:                              "tls·ws",
	}
	for j, want := range ob {
		if got := OutboundProtoTag([]byte(j)); got != want {
			t.Errorf("OutboundProtoTag = %q, want %q", got, want)
		}
	}
}
