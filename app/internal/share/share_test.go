package share

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"clashvless/internal/store"
)

const (
	linkA = "vless://11111111-1111-1111-1111-111111111111@a.example:443?security=reality&sni=a&pbk=k&type=tcp#A"
	linkB = "vless://22222222-2222-2222-2222-222222222222@b.example:443?security=none&type=grpc&serviceName=s#B"
	linkC = "vless://33333333-3333-3333-3333-333333333333@c.example:443?security=tls&sni=c&type=ws&path=%2F#C"
)

func TestParseExitsAcceptsSubscriptionBlob(t *testing.T) {
	links, invalid := ParseExits(Subscription([]Link{{URL: linkA}, {URL: linkB}}))
	if invalid != 0 || len(links) != 2 || links[0] != linkA || links[1] != linkB {
		t.Fatalf("links %v, invalid %d", links, invalid)
	}
	links, invalid = ParseExits(linkA + " " + linkC)
	if invalid != 0 || len(links) != 2 {
		t.Fatalf("two links on one line: %v, invalid %d", links, invalid)
	}
}

func TestNodeLinks(t *testing.T) {
	links, skipped := NodeLinks([]store.Node{
		{Name: "raw", Raw: linkB},
		{Name: "json", Outbound: []byte(`{"protocol":"vless","settings":{"vnext":[{"address":"j.example","port":443,"users":[{"id":"u","encryption":"none"}]}]},"streamSettings":{"network":"tcp","security":"none","sockopt":{"mark":1}}}`)},
		{Name: "trojan", Outbound: []byte(`{"protocol":"trojan"}`)},
	})
	if skipped != 1 || len(links) != 2 || links[0].URL != linkB {
		t.Fatalf("links %+v, skipped %d", links, skipped)
	}
	if !strings.HasPrefix(links[1].URL, "vless://u@j.example:443?") || !strings.Contains(links[1].Note, "sockopt") {
		t.Fatalf("converted link %+v", links[1])
	}
}

func TestServe(t *testing.T) {
	request := `{"mains":[{"name":"Cloud exit","url":"` + linkC + `","enabled":true}],
		"nodes":[{"name":"raw","raw":"` + linkB + `"},{"name":"trojan","outbound":{"protocol":"trojan"}}],
		"parse":"` + linkA + `\nnot a link\n` + linkB + `"}`
	var out bytes.Buffer
	if err := Serve(strings.NewReader(request), &out); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Links) != 2 || response.Skipped != 1 {
		t.Fatalf("links %+v, skipped %d", response.Links, response.Skipped)
	}
	if got := response.Links[0].URL; !strings.HasSuffix(got, "#Cloud%20exit") {
		t.Fatalf("exit link = %s", got)
	}
	code, err := QR(response.Links[1].URL)
	if err != nil {
		t.Fatal(err)
	}
	qr := response.Links[1].QR
	if qr == nil || qr.Size != code.Size || len(qr.Modules) != code.Size*code.Size || (qr.Modules[0] == '1') != code.Black(0, 0) {
		t.Fatalf("qr %+v", qr)
	}
	if response.Invalid != 1 || len(response.Exits) != 2 {
		t.Fatalf("exits %+v, invalid %d", response.Exits, response.Invalid)
	}
	if e := response.Exits[0]; e.Name != "A" || e.URL != linkA || !e.Enabled || !e.AllowNoHop {
		t.Fatalf("reality exit %+v", e)
	}
	if e := response.Exits[1]; e.Name != "B" || e.AllowNoHop {
		t.Fatalf("plain exit %+v", e)
	}
}

func TestTooLongForQR(t *testing.T) {
	response := Handle(Request{Mains: []store.Main{{Name: "x", URL: strings.Replace(linkA, "#A", "&p="+strings.Repeat("x", 4000)+"#A", 1)}}})
	if len(response.Links) != 1 || response.Links[0].QR != nil {
		t.Fatalf("links %+v", response.Links)
	}
}

func TestParseProxyExitsAndBuildProxy(t *testing.T) {
	raw := "http://u:p@1.2.3.4:8080#HTTP1\n5.6.7.8:1080:bob:pw\nnot-valid\n"
	links, invalid := ParseProxyExits(raw, "socks5")
	if invalid != 1 || len(links) != 2 {
		t.Fatalf("links=%v invalid=%d", links, invalid)
	}
	if links[0] != "http://u:p@1.2.3.4:8080#HTTP1" || links[1] != "socks5://bob:pw@5.6.7.8:1080" {
		t.Fatalf("unexpected formatted links: %v", links)
	}

	// Serve with slot_type="proxy"
	req := `{"parse":"1.2.3.4:3128:u:p\nhttp://9.9.9.9:8080#Web","slot_type":"proxy","default_scheme":"http"}`
	var out bytes.Buffer
	if err := Serve(strings.NewReader(req), &out); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Invalid != 0 || len(resp.Exits) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Exits[0].URL != "http://u:p@1.2.3.4:3128" || !resp.Exits[0].AllowNoHop {
		t.Fatalf("first exit = %+v", resp.Exits[0])
	}

	// Serve with build_proxy (manual parameters)
	out.Reset()
	req = `{"build_proxy":{"scheme":"socks5","host":"proxy.example","port":1080,"user":"alice","pass":"pw","name":"EU"}}`
	if err := Serve(strings.NewReader(req), &out); err != nil {
		t.Fatal(err)
	}
	resp = Response{}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" || len(resp.Exits) != 1 || resp.Exits[0].URL != "socks5://alice:pw@proxy.example:1080#EU" || resp.Exits[0].Name != "EU" {
		t.Fatalf("build_proxy resp = %+v", resp)
	}
}

func TestServeSub(t *testing.T) {
	subURL := "https://example.com/sub/mytoken"
	response := Handle(Request{
		Sub:   &Link{Name: "My Sub", URL: subURL},
		Nodes: []store.Node{{Name: "raw", Raw: linkB}},
	})
	if response.Sub == nil || response.Sub.URL != subURL || response.Sub.Name != "My Sub" {
		t.Fatalf("sub = %+v", response.Sub)
	}
	if response.Sub.QR == nil {
		t.Fatal("expected QR for sub")
	}
	if len(response.Links) != 1 || response.Links[0].URL != linkB {
		t.Fatalf("links = %+v", response.Links)
	}
}

func TestServeHWID(t *testing.T) {
	hwid := "abcdef12-3456-7890-abcd-ef1234567890"
	response := Handle(Request{HWID: hwid})
	if len(response.Links) != 1 || response.Links[0].URL != hwid || response.Links[0].Name != "Hardware ID" {
		t.Fatalf("hwid response = %+v", response.Links)
	}
	if response.Links[0].QR == nil {
		t.Fatal("expected QR for HWID")
	}
}
