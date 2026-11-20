//go:build !android && cgo

package gui

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"clashvless/internal/share"
	"clashvless/internal/store"
)

func (d *dashboard) settingsView() fyne.CanvasObject {
	sections := []string{"Network & failover", "VPN", "Subscriptions", "Device"}
	selector := widget.NewSelect(sections, nil)
	selector.SetSelected(d.settingsSection)
	selector.OnChanged = func(section string) {
		d.settingsSection = section
		d.render()
	}

	var content fyne.CanvasObject
	switch d.settingsSection {
	case "VPN":
		vpnItems := []fyne.CanvasObject{
			d.boolSetting("Enable VPN", "tun_enabled", d.state.TunEnabled, true, false),
		}
		if d.state != nil && len(d.state.Slots) > 0 {
			slotNames := make([]string, len(d.state.Slots))
			for i, slot := range d.state.Slots {
				slotNames[i] = slot.Name
			}
			vpnItems = append(vpnItems, d.selectSetting("Exit slot", "tun_slot", d.state.TunSlotName(), slotNames, nil))
		}
		vpnItems = append(vpnItems,
			d.selectSetting("DNS mode", "tun_dns_direct", strconv.FormatBool(d.state.TunDNSDirect), []string{"false", "true"}, map[string]string{"false": "Static routed", "true": "Real network"}),
			d.textSetting("Static DNS resolver", "tun_static_dns", d.state.TunStaticDNS, "8.8.8.8"),
			d.boolSetting("Tunnel private/LAN ranges", "tun_tunnel_lan", d.state.TunTunnelLAN, true, false),
			d.boolSetting("Allow IPv6 outside VPN", "tun_allow_ipv6", d.state.TunAllowIPv6, true, false),
			d.boolSetting("Answer ping locally", "tun_no_icmp", d.state.TunICMP(), false, true),
		)
		content = container.NewVBox(
			panel("System-wide VPN", "Capture device traffic through the selected exit slot", vpnItems...),
			panel("Virtual interface", "Changes apply when the VPN reconnects",
				d.textSetting("Device name", "tun_name", d.state.TunName, "Platform default"),
				d.textSetting("Interface address", "tun_addr", d.state.TunAddr, "198.18.0.1/30"),
				d.numberSetting("MTU · 0 = default", "tun_mtu", d.state.TunMTU, 576, 9000, true),
			),
		)
	case "Subscriptions":
		content = panel("Subscription updates", "Background refresh and optional fetch proxy",
			d.boolSetting("Automatic refresh", "no_auto_fetch", d.state.AutoFetch(), false, true),
			d.numberSetting("Refresh interval · hours", "fetch_interval_h", effective(d.state.FetchIntervalH, 24), 1, 168, false),
			d.boolSetting("Use fetch proxy", "use_fetch_proxy", d.state.UseFetchProxy, true, false),
			d.textSetting("Fetch proxy", "fetch_proxy", d.state.FetchProxy, "socks5://127.0.0.1:2084"),
		)
	case "Device":
		content = panel("Device identity", "Stable Happ-compatible identity shared by every subscription",
			d.textSetting("Hardware ID", "device_hwid", d.state.Device.HWID, ""),
			widget.NewSeparator(),
			infoRow("Panel OS", d.state.Device.OS+" "+d.state.Device.OSVer),
			infoRow("Model", d.state.Device.Model),
			infoRow("User agent", d.state.Device.UA),
			infoRow("Locale", d.state.Device.Locale),
		)
	default:
		content = container.NewVBox(
			panel("Automatic failover", "Route selection, health checks, and recovery",
				d.selectSetting("Pin tier", "pin_tier", strconv.Itoa(d.state.PinTier), []string{"0", "1", "2", "3"}, map[string]string{"0": "Automatic", "1": "T1 · Direct", "2": "T2 · Country", "3": "T3 · Bypass"}),
				d.boolSetting("Force a hop · skip T1", "force_hop", d.state.ForceHop, true, false),
				d.numberSetting("Probe interval · seconds", "interval_s", effective(d.state.Interval, 12), 2, 600, false),
				d.numberSetting("Egress timeout · seconds", "timeout_s", effective(d.state.Timeout, 6), 1, 60, false),
				d.numberSetting("Recovery threshold", "up_threshold", effective(d.state.UpThreshold, 3), 1, 10, false),
				d.numberSetting("Failure threshold", "down_threshold", effective(d.state.DownThreshold, 2), 1, 10, false),
			),
			panel("Local listeners", "Each slot's SOCKS5 port is set in Exit routes. Listener and log changes apply after a daemon restart",
				d.numberSetting("First-hop port · 0 = auto", "entry_port", d.state.EntryPort, 1024, 65535, true),
				d.boolSetting("Allow LAN connections", "listen_addr", d.state.ListenHost() == "0.0.0.0", "0.0.0.0", "127.0.0.1"),
				d.selectSetting("Xray log level", "log_level", d.state.Loglevel(), []string{"none", "error", "warning", "info", "debug"}, nil),
			),
		)
	}
	return container.NewVBox(
		flexRow(widget.NewLabel("Section"), container.New(&widthLayout{width: 200}, selector)),
		content,
	)
}

func settingRow(name string, control fyne.CanvasObject) fyne.CanvasObject {
	label := widget.NewLabel(name)
	label.Truncation = fyne.TextTruncateEllipsis
	return flexRow(label, control)
}

func infoRow(name, value string) fyne.CanvasObject {
	return container.NewVBox(muted(name), ellipsized(value))
}

// settingEntry is a text field whose unsaved text survives page rebuilds.
func (d *dashboard) settingEntry(key, current, placeholder string) *widget.Entry {
	text, drafted := d.drafts[key]
	if !drafted {
		text = current
	}
	return d.textInput("setting:"+key, placeholder, text, func(value string) {
		if value == current {
			delete(d.drafts, key)
		} else {
			d.drafts[key] = value
		}
	})
}

func (d *dashboard) saveSetting(key string, value any) {
	delete(d.drafts, key)
	d.patch(map[string]any{key: value})
}

// numberSetting accepts a value in min..max, or 0 (the default) when zeroOK.
func (d *dashboard) numberSetting(name, key string, current, min, max int, zeroOK bool) fyne.CanvasObject {
	entry := d.settingEntry(key, strconv.Itoa(current), "")
	save := widget.NewButtonWithIcon("", theme.ConfirmIcon(), func() {
		value, err := strconv.Atoi(strings.TrimSpace(entry.Text))
		if err != nil || (value < min || value > max) && !(zeroOK && value == 0) {
			if zeroOK {
				d.notify(fmt.Sprintf("%s must be 0 or between %d and %d", name, min, max))
			} else {
				d.notify(fmt.Sprintf("%s must be between %d and %d", name, min, max))
			}
			return
		}
		if key == "entry_port" && d.state.EntryPortConflict(value) {
			d.notify("That port is already used by a slot or its first hop")
			return
		}
		d.saveSetting(key, value)
	})
	entry.OnSubmitted = func(string) { save.OnTapped() }
	return settingRow(name, container.NewBorder(nil, nil, nil, save, container.New(&widthLayout{width: 180}, entry)))
}

func validateText(key, value string) string {
	switch key {
	case "device_hwid":
		if !store.ValidHWID(value) {
			return "HWID must be 10–64 letters, digits, = or -"
		}
	case "tun_static_dns":
		if value != "" && net.ParseIP(value) == nil {
			return "DNS resolver must be an IP address"
		}
	case "tun_addr":
		if value != "" {
			if ip, _, err := net.ParseCIDR(value); err != nil || ip.To4() == nil {
				return "Interface address must be an IPv4 CIDR such as 198.18.0.1/30"
			}
		}
	case "tun_name":
		if strings.ContainsAny(value, " \t/") {
			return "Device name cannot contain spaces or /"
		}
	}
	return ""
}

func (d *dashboard) textSetting(name, key, current, placeholder string) fyne.CanvasObject {
	entry := d.settingEntry(key, current, placeholder)
	save := widget.NewButtonWithIcon("", theme.ConfirmIcon(), func() {
		value := strings.TrimSpace(entry.Text)
		if problem := validateText(key, value); problem != "" {
			d.notify(problem)
			return
		}
		if key == "device_hwid" {
			device := d.state.Device
			device.HWID = value
			delete(d.drafts, key)
			d.patch(map[string]any{"device": device})
			return
		}
		d.saveSetting(key, value)
	})
	entry.OnSubmitted = func(string) { save.OnTapped() }
	var actions fyne.CanvasObject = save
	if key == "device_hwid" {
		qrBtn := widget.NewButtonWithIcon("", qrIcon(), func() {
			val := strings.TrimSpace(entry.Text)
			if val == "" {
				val = d.state.Device.HWID
			}
			if val == "" {
				d.notify("No HWID configured")
				return
			}
			d.showShare("Export · Hardware ID", []share.Link{{Name: "Hardware ID", URL: val}}, 0)
		})
		actions = container.NewHBox(save, qrBtn)
	}
	return settingRow(name, container.NewBorder(nil, nil, nil, actions, container.New(&widthLayout{width: 180}, entry)))
}

func (d *dashboard) selectSetting(name, key, current string, options []string, labels map[string]string) fyne.CanvasObject {
	valuesByLabel := map[string]string{}
	displayOptions := make([]string, len(options))
	currentLabel := current
	for i, value := range options {
		label := value
		if labels != nil {
			label = labels[value]
		}
		displayOptions[i] = label
		valuesByLabel[label] = value
		if value == current {
			currentLabel = label
		}
	}
	selectWidget := widget.NewSelect(displayOptions, nil)
	selectWidget.SetSelected(currentLabel)
	selectWidget.OnChanged = func(label string) {
		value := valuesByLabel[label]
		if key == "pin_tier" {
			valueNumber, _ := strconv.Atoi(value)
			d.patch(map[string]any{key: valueNumber})
			return
		}
		if key == "tun_dns_direct" {
			d.patch(map[string]any{key: value == "true"})
			return
		}
		d.patch(map[string]any{key: value})
	}
	return settingRow(name, selectWidget)
}

func (d *dashboard) boolSetting(name, key string, checked bool, onValue, offValue any) fyne.CanvasObject {
	check := widget.NewCheck("", nil)
	check.Checked = checked
	check.OnChanged = func(on bool) {
		value := offValue
		if on {
			value = onValue
		}
		d.patch(map[string]any{key: value})
	}
	return settingRow(name, check)
}

func effective(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}
