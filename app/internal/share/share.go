// Package share turns exits and entry nodes into VLESS links and QR codes, and
// parses pasted exit links. The desktop GUI and the Android app (through the
// hidden `share` command) use it, so both export the same text.
package share

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"strings"

	"rsc.io/qr"

	"clashvless/internal/store"
	"clashvless/internal/xray"
)

type Link struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Note string `json:"note,omitempty"` // why the link differs from the stored config, if it does
}

// MainLink names the exported link after the exit as it is shown in the app.
func MainLink(main store.Main) Link {
	link := main.URL
	if parsed, err := url.Parse(main.URL); err == nil && strings.TrimSpace(main.Name) != "" {
		parsed.Fragment, parsed.RawFragment = strings.TrimSpace(main.Name), ""
		link = parsed.String()
	}
	return Link{Name: main.Name, URL: link}
}

func NodeLink(node store.Node) (Link, error) {
	if strings.HasPrefix(strings.ToLower(node.Raw), "vless://") {
		return Link{Name: node.Name, URL: node.Raw}, nil
	}
	link, dropped, err := xray.OutboundToVLESS(node.Outbound, node.Name)
	if err != nil {
		return Link{}, err
	}
	note := ""
	if len(dropped) > 0 {
		note = "Not representable in a link, left out: " + strings.Join(dropped, ", ")
	}
	return Link{Name: node.Name, URL: link, Note: note}, nil
}

// NodeLinks exports every node it can; skipped counts the non-VLESS ones.
func NodeLinks(nodes []store.Node) (links []Link, skipped int) {
	for _, node := range nodes {
		link, err := NodeLink(node)
		if err != nil {
			skipped++
			continue
		}
		links = append(links, link)
	}
	return links, skipped
}

func Joined(links []Link) string {
	urls := make([]string, len(links))
	for i, link := range links {
		urls[i] = link.URL
	}
	return strings.Join(urls, "\n")
}

func Subscription(links []Link) string {
	return base64.StdEncoding.EncodeToString([]byte(Joined(links)))
}

// QR encodes text, dropping to the lowest error correction when it is too long.
func QR(text string) (*qr.Code, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		code, err = qr.Encode(text, qr.L)
	}
	return code, err
}

// ParseExits takes links one per line (or several on a line), or a base64 subscription.
func ParseExits(text string) (links []string, invalid int) {
	text = strings.TrimSpace(text)
	if text != "" && !strings.Contains(strings.ToLower(text), "://") && len(strings.Fields(text)) == 1 {
		for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if decoded, err := encoding.DecodeString(text); err == nil {
				text = string(decoded)
				break
			}
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		candidates := []string{line}
		if strings.Count(strings.ToLower(line), "vless://") > 1 {
			candidates = strings.Fields(line)
		}
		for _, candidate := range candidates {
			if _, err := xray.VlessToOutbound(candidate, "main"); err != nil {
				invalid++
				continue
			}
			links = append(links, candidate)
		}
	}
	return links, invalid
}

// ParseProxyExits takes HTTP/SOCKS5 proxies one per line — either standard URLs
// (http://, socks5://) or common proxy formats (host:port:user:pass,
// user:pass@host:port, host:port) — or a base64 list, and returns canonical URLs.
func ParseProxyExits(text, defaultScheme string) (links []string, invalid int) {
	text = strings.TrimSpace(text)
	if text != "" && !strings.ContainsAny(text, "://@") && len(strings.Fields(text)) == 1 {
		for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if decoded, err := encoding.DecodeString(text); err == nil {
				text = string(decoded)
				break
			}
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		spec, err := xray.ParseProxy(line, defaultScheme)
		if err != nil {
			invalid++
			continue
		}
		formatted, err := xray.FormatProxyURL(spec)
		if err != nil {
			invalid++
			continue
		}
		links = append(links, formatted)
	}
	return links, invalid
}

// Request is what the Android app sends the `share` command on stdin.
type Request struct {
	Mains         []store.Main    `json:"mains,omitempty"`
	Nodes         []store.Node    `json:"nodes,omitempty"`
	Sub           *Link           `json:"sub,omitempty"`
	Links         []Link          `json:"links,omitempty"`
	HWID          string          `json:"hwid,omitempty"`
	Parse         string          `json:"parse,omitempty"`
	SlotType      string          `json:"slot_type,omitempty"`
	DefaultScheme string          `json:"default_scheme,omitempty"`
	BuildProxy    *xray.ProxySpec `json:"build_proxy,omitempty"`
}

type Code struct {
	Size    int    `json:"size"`
	Modules string `json:"modules"` // Size*Size of '1' (dark) and '0', row by row; no quiet zone
}

type Item struct {
	Link
	QR *Code `json:"qr,omitempty"` // nil when the link is too long for a QR code
}

type Response struct {
	Sub     *Item        `json:"sub,omitempty"`
	Links   []Item       `json:"links"`
	Skipped int          `json:"skipped,omitempty"`
	Exits   []store.Main `json:"exits,omitempty"`
	Invalid int          `json:"invalid,omitempty"`
	Error   string       `json:"error,omitempty"`
}

func qrItem(link Link) Item {
	item := Item{Link: link}
	if code, err := QR(link.URL); err == nil {
		var modules strings.Builder
		modules.Grow(code.Size * code.Size)
		for y := 0; y < code.Size; y++ {
			for x := 0; x < code.Size; x++ {
				if code.Black(x, y) {
					modules.WriteByte('1')
				} else {
					modules.WriteByte('0')
				}
			}
		}
		item.QR = &Code{Size: code.Size, Modules: modules.String()}
	}
	return item
}

func Handle(request Request) Response {
	links := make([]Link, 0, len(request.Mains)+len(request.Nodes)+len(request.Links)+1)
	links = append(links, request.Links...)
	if request.HWID != "" {
		links = append(links, Link{Name: "Hardware ID", URL: request.HWID})
	}
	for _, main := range request.Mains {
		links = append(links, MainLink(main))
	}
	nodes, skipped := NodeLinks(request.Nodes)
	links = append(links, nodes...)
	response := Response{Links: make([]Item, len(links)), Skipped: skipped}
	if request.Sub != nil {
		sub := qrItem(*request.Sub)
		response.Sub = &sub
	}
	for i, link := range links {
		response.Links[i] = qrItem(link)
	}
	if request.BuildProxy != nil {
		formatted, err := xray.FormatProxyURL(*request.BuildProxy)
		if err != nil {
			response.Error = err.Error()
		} else {
			slot := store.Slot{Type: store.SlotTypeProxy}
			slot.AddMain(formatted)
			response.Exits = slot.Mains
		}
		return response
	}
	if request.Parse != "" {
		if request.SlotType == store.SlotTypeProxy {
			parsed, invalid := ParseProxyExits(request.Parse, request.DefaultScheme)
			slot := store.Slot{Type: store.SlotTypeProxy}
			for _, link := range parsed {
				slot.AddMain(link)
			}
			response.Exits, response.Invalid = slot.Mains, invalid
		} else {
			parsed, invalid := ParseExits(request.Parse)
			var slot store.Slot
			for _, link := range parsed {
				slot.AddMain(link)
			}
			response.Exits, response.Invalid = slot.Mains, invalid
		}
	}
	return response
}

// Serve answers one Request read from in.
func Serve(in io.Reader, out io.Writer) error {
	var request Request
	if err := json.NewDecoder(in).Decode(&request); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(Handle(request))
}
