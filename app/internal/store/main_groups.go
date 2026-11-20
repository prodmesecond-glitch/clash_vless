package store

import (
	"net"
	"net/url"
)

// MainGroupKey ignores only the server address and display name. Credentials,
// port and every transport/security parameter must match; DNS is never resolved.
func MainGroupKey(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "vless" || u.User == nil || u.User.Username() == "" || u.Hostname() == "" || u.Port() == "" {
		return link
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return link
	}
	u.Host = net.JoinHostPort("server", u.Port())
	u.Fragment, u.RawFragment = "", ""
	u.RawQuery = q.Encode()
	return u.String()
}

// MainGroups returns indices in group priority order, preserving address order
// inside each group and leaving the stored slice unchanged.
func MainGroups(mains []Main) [][]int {
	var groups [][]int
	byKey := make(map[string]int)
	for i, main := range mains {
		key := MainGroupKey(main.URL)
		g, found := byKey[key]
		if !found {
			g = len(groups)
			byKey[key] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], i)
	}
	return groups
}

func (sl *Slot) priorityMains() []Main {
	var mains []Main
	for _, group := range MainGroups(sl.Mains) {
		for _, index := range group {
			mains = append(mains, sl.Mains[index])
		}
	}
	return mains
}
