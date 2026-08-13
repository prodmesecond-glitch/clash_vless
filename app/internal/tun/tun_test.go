package tun

import "testing"

func TestCIDRIPMask(t *testing.T) {
	cases := []struct{ cidr, ip, mask string }{
		{"198.18.0.1/30", "198.18.0.1", "255.255.255.252"},
		{"10.0.0.1/24", "10.0.0.1", "255.255.255.0"},
		{"172.16.5.9/16", "172.16.5.9", "255.255.0.0"},
	}
	for _, c := range cases {
		ip, mask, err := cidrIPMask(c.cidr)
		if err != nil || ip != c.ip || mask != c.mask {
			t.Errorf("cidrIPMask(%q) = %q, %q, %v; want %q, %q", c.cidr, ip, mask, err, c.ip, c.mask)
		}
	}
	if _, _, err := cidrIPMask("not-a-cidr"); err == nil {
		t.Error("expected error for invalid CIDR")
	}
}
