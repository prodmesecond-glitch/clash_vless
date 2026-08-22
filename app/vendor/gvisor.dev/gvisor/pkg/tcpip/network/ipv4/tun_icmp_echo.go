package ipv4

import "sync/atomic"

// ReplyToTemporaryEcho, when set, makes handleICMP answer an ICMP Echo request
// delivered to a *temporary* (promiscuous-mode) local address instead of
// skipping it. clashvless enables this for its TUN netstack so `ping` works:
// every remote destination captured by the tun arrives on a promiscuous
// temporary address, which gVisor otherwise leaves unanswered (handing it to a
// custom transport handler that xray-core's tun inbound never installs). The
// reply is synthesized locally by gVisor's own echo-reply path (see icmp.go) —
// it proves the tun stack is live, NOT end-to-end exit reachability (ICMP can't
// traverse the SOCKS/VLESS tunnel; use curl for that).
//
// LOCAL VENDORED PATCH (grep "clashvless") — re-apply after any xray-core/gVisor
// bump re-vendors this tree.
var ReplyToTemporaryEcho atomic.Bool
