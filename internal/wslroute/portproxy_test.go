package wslroute

import (
	"strconv"
	"testing"
)

const (
	hostAddress  = "192.168.1.31"
	guestAddress = "172.25.172.58"
	meshPort     = 7521
)

func proxyRow(listen string, listenPort int, connect string, connectPort int) string {
	return listen + "  " + strconv.Itoa(listenPort) + "  " + connect + "  " + strconv.Itoa(connectPort)
}

func TestPortproxyStatusRequiresExactRoute(t *testing.T) {
	cases := []struct {
		name string
		row  string
		want string
	}{
		{"exact match", proxyRow(hostAddress, meshPort, guestAddress, meshPort), "present"},
		{"wrong listen host", proxyRow("192.168.1.99", meshPort, guestAddress, meshPort), "absent"},
		{"wrong private guest", proxyRow(hostAddress, meshPort, "172.25.172.59", meshPort), "absent"},
		{"wrong listen port", proxyRow(hostAddress, meshPort+1, guestAddress, meshPort), "absent"},
		{"wrong connect port", proxyRow(hostAddress, meshPort, guestAddress, meshPort+1), "absent"},
		{"loopback target", proxyRow(hostAddress, meshPort, "127.0.0.1", meshPort), "absent"},
		{"public target", proxyRow(hostAddress, meshPort, "8.8.8.8", meshPort), "absent"},
		{"broadcast target", proxyRow(hostAddress, meshPort, "255.255.255.255", meshPort), "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PortproxyStatus("Listen Address  Port  Connect Address  Port\r\n"+tc.row, hostAddress, meshPort, guestAddress)
			if got != tc.want {
				t.Fatalf("PortproxyStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPortproxyStatusSearchesAllRows(t *testing.T) {
	text := "Listen Address  Port  Connect Address  Port\r\n" +
		proxyRow("192.168.1.99", meshPort, guestAddress, meshPort) + "\r\n" +
		proxyRow(hostAddress, meshPort, "127.0.0.1", meshPort) + "\r\n" +
		proxyRow(hostAddress, meshPort, guestAddress, meshPort)
	if got := PortproxyStatus(text, hostAddress, meshPort, guestAddress); got != "present" {
		t.Fatalf("PortproxyStatus() = %q, want present with a later exact row", got)
	}
}

func TestPortproxyStatusUnresolvedGuest(t *testing.T) {
	matching := "Listen Address  Port  Connect Address  Port\r\n" + proxyRow(hostAddress, meshPort, guestAddress, meshPort)
	if got := PortproxyStatus(matching, hostAddress, meshPort, ""); got != "unknown" {
		t.Fatalf("matching row with unresolved guest = %q, want unknown", got)
	}
	if got := PortproxyStatus("Listen Address  Port  Connect Address  Port\r\n", hostAddress, meshPort, ""); got != "absent" {
		t.Fatalf("missing row with unresolved guest = %q, want absent", got)
	}
}

func TestPortproxyStatusRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		host  string
		port  int
		guest string
	}{
		{"not-an-ip", meshPort, guestAddress},
		{hostAddress, 0, guestAddress},
		{hostAddress, meshPort, "not-an-ip"},
	} {
		if got := PortproxyStatus(proxyRow(hostAddress, meshPort, guestAddress, meshPort), tc.host, tc.port, tc.guest); got != "unknown" {
			t.Errorf("invalid config (%q,%d,%q) = %q, want unknown", tc.host, tc.port, tc.guest, got)
		}
	}
}

func TestIsGuestIPv4(t *testing.T) {
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{"10.0.0.2", true},
		{"172.16.0.1", true},
		{"172.31.255.254", true},
		{"192.168.1.2", true},
		{"172.25.1.0", false},
		{"172.25.1.255", false},
		{"172.32.0.1", false},
		{"127.0.0.1", false},
		{"169.254.1.1", false},
		{"8.8.8.8", false},
		{"0.0.0.0", false},
		{"255.255.255.255", false},
		{"224.0.0.1", false},
		{"::1", false},
		{"::ffff:192.168.1.2", false},
	} {
		if got := IsGuestIPv4(tc.address); got != tc.want {
			t.Errorf("IsGuestIPv4(%q) = %t, want %t", tc.address, got, tc.want)
		}
	}
}
