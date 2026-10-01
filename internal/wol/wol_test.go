package wol

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestMagicPacket(t *testing.T) {
	mac, _ := net.ParseMAC("a4:bb:6d:12:34:56")
	pkt, err := MagicPacket(mac)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkt) != 102 {
		t.Fatalf("len = %d, want 102", len(pkt))
	}
	if !bytes.Equal(pkt[:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) {
		t.Fatalf("sync stream = % x", pkt[:6])
	}
	for i := range 16 {
		if got := pkt[6+6*i : 12+6*i]; !bytes.Equal(got, mac) {
			t.Fatalf("repetition %d = % x, want % x", i, got, mac)
		}
	}
	if _, err := MagicPacket(net.HardwareAddr{1, 2, 3}); err == nil {
		t.Fatal("short MAC accepted")
	}
}

func TestBroadcastAddr(t *testing.T) {
	for _, tc := range []struct {
		prefix, want string
	}{
		{"192.168.1.20/24", "192.168.1.255"},
		{"10.1.2.3/8", "10.255.255.255"},
		{"172.16.5.4/20", "172.16.15.255"},
		{"192.168.7.9/30", "192.168.7.11"},
		{"192.168.7.9/31", ""},
		{"192.168.7.9/32", ""},
		{"fd00::1/64", ""},
	} {
		got, ok := BroadcastAddr(netip.MustParsePrefix(tc.prefix))
		if tc.want == "" {
			if ok {
				t.Errorf("%s: got %s, want none", tc.prefix, got)
			}
			continue
		}
		if !ok || got.String() != tc.want {
			t.Errorf("%s: got %v %v, want %s", tc.prefix, got, ok, tc.want)
		}
	}
}

func mustMAC(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

func TestWakeableFiltersVirtualAndDown(t *testing.T) {
	up := net.FlagUp | net.FlagBroadcast | net.FlagMulticast
	lan := []netip.Prefix{netip.MustParsePrefix("192.168.1.20/24")}
	ifs := []Iface{
		{Name: "Ethernet", Desc: "Intel(R) Ethernet Controller I225-V", Flags: up, MAC: mustMAC("a4:bb:6d:00:00:01"), Addrs: lan},
		{Name: "Wi-Fi", Desc: "Realtek 8852CE WiFi 6E PCI-E NIC", Flags: up, MAC: mustMAC("a4:bb:6d:00:00:02"),
			Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.21/24"), netip.MustParsePrefix("169.254.3.4/16")}},
		{Name: "vEthernet (WSL)", Desc: "Hyper-V Virtual Ethernet Adapter", Flags: up, MAC: mustMAC("00:15:5d:00:00:03"), Addrs: lan},
		{Name: "Ethernet 3", Desc: "TAP-Windows Adapter V9", Flags: up, MAC: mustMAC("00:ff:00:00:00:04")},
		{Name: "Local Area Connection* 2", Desc: "Microsoft Wi-Fi Direct Virtual Adapter", Flags: up, MAC: mustMAC("a6:bb:6d:00:00:05")},
		{Name: "docker0", Flags: up, MAC: mustMAC("02:42:ac:00:00:06")},
		{Name: "br0", Flags: up, MAC: mustMAC("52:54:00:00:00:07"), Virtual: true, Addrs: lan},
		{Name: "tailscale0", Flags: up, MAC: mustMAC("52:54:00:00:00:08")},
		{Name: "wg0", Flags: up},
		{Name: "lo", Flags: up | net.FlagLoopback},
		// eth1 is down; enp3s0 is a bridge port without IPv4 but still wakeable.
		{Name: "eth1", Flags: net.FlagBroadcast, MAC: mustMAC("a4:bb:6d:00:00:09")},
		{Name: "enp3s0", Flags: up, MAC: mustMAC("a4:bb:6d:00:00:0a")},
		{Name: "Ethernet 4", Desc: "Some NIC", Flags: up, MAC: mustMAC("a4:bb:6d:00:00:0b"), Virtual: true},
	}
	got := Wakeable(ifs)
	var names []string
	for _, tg := range got {
		names = append(names, tg.Interface)
	}
	if want := []string{"Ethernet", "Wi-Fi", "enp3s0"}; !slices.Equal(names, want) {
		t.Fatalf("wakeable = %v, want %v", names, want)
	}
	wifi := got[1]
	if wifi.MAC != "a4:bb:6d:00:00:02" || len(wifi.Subnets) != 2 ||
		wifi.Subnets[0].Broadcast.String() != "192.168.1.255" || wifi.Subnets[1].Broadcast.String() != "169.254.255.255" {
		t.Fatalf("wifi target = %+v", wifi)
	}
	if len(got[2].Subnets) != 0 {
		t.Fatalf("bridge port should have no subnets: %+v", got[2])
	}
	srcs := sources(ifs)
	if len(srcs) != 3 || srcs[0].Interface != "Ethernet" || srcs[1].Prefix.String() != "192.168.1.21/24" {
		t.Fatalf("sources = %+v", srcs) // enp3s0 has no IPv4 and no broadcast flag: not a source
	}
}

func TestSanitize(t *testing.T) {
	in := []Target{
		{MAC: "A4-BB-6D-12-34-56", Interface: "eth0", Subnets: []Subnet{
			{Prefix: netip.MustParsePrefix("192.168.1.20/24"), Broadcast: netip.MustParseAddr("8.8.8.8")},
			{Prefix: netip.MustParsePrefix("192.168.9.9/32")},
		}},
		// Dropped: duplicate, multicast, zero, garbage, EUI-64.
		{MAC: "a4:bb:6d:12:34:56"},
		{MAC: "01:00:5e:00:00:01"},
		{MAC: "00:00:00:00:00:00"},
		{MAC: "not a mac"},
		{MAC: "00:11:22:33:44:55:66:77"},
		{MAC: "00:11:22:33:44:55", Interface: "wlan0"},
	}
	got := Sanitize(in)
	if len(got) != 2 || got[0].MAC != "a4:bb:6d:12:34:56" || got[1].Interface != "wlan0" {
		t.Fatalf("sanitized = %+v", got)
	}
	if len(got[0].Subnets) != 1 || got[0].Subnets[0].Broadcast.String() != "192.168.1.255" {
		t.Fatalf("subnets = %+v, want only 192.168.1.0/24 with its real broadcast", got[0].Subnets)
	}
	many := make([]Target, 20)
	for i := range many {
		many[i] = Target{MAC: net.HardwareAddr{0x02, 0, 0, 0, 0, byte(i + 1)}.String()}
	}
	if n := len(Sanitize(many)); n != maxTargets {
		t.Fatalf("kept %d targets, want cap %d", n, maxTargets)
	}
}

func TestPlanRoutes(t *testing.T) {
	targets := []Target{
		{MAC: "a4:bb:6d:00:00:01", Subnets: []Subnet{{Prefix: netip.MustParsePrefix("192.168.1.20/24"), Broadcast: netip.MustParseAddr("192.168.1.255")}}},
		{MAC: "a4:bb:6d:00:00:02", Subnets: []Subnet{{Prefix: netip.MustParsePrefix("10.0.0.7/24"), Broadcast: netip.MustParseAddr("10.0.0.255")}}},
	}
	locals := []Local{
		{Interface: "eth0", Prefix: netip.MustParsePrefix("192.168.1.5/24")},
		{Interface: "wlan0", Prefix: netip.MustParsePrefix("172.20.0.3/16")},
	}
	dgs, noShared, err := plan(targets, locals, LimitedBroadcast)
	if err != nil {
		t.Fatal(err)
	}
	if noShared {
		t.Fatal("eth0 shares 192.168.1.0/24 with the first NIC")
	}
	var got []string
	for _, d := range dgs {
		got = append(got, d.src.Interface+">"+d.dst.String())
	}
	want := []string{
		// First MAC: directed broadcast from eth0 plus the limited broadcast everywhere.
		"eth0>192.168.1.255", "eth0>255.255.255.255", "wlan0>255.255.255.255",
		// Second MAC: no local interface in 10.0.0.0/24.
		"eth0>255.255.255.255", "wlan0>255.255.255.255",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	mac2, _ := MagicPacket(mustMAC("a4:bb:6d:00:00:02"))
	if !bytes.Equal(dgs[4].payload, mac2) {
		t.Fatal("second MAC's datagrams carry the wrong payload")
	}

	// No local interface in any target subnet: only the limited broadcast, flagged.
	_, noShared, _ = plan(targets[1:], locals, LimitedBroadcast)
	if !noShared {
		t.Fatal("no shared subnet not reported")
	}
	// No usable local address at all: one unbound limited broadcast per MAC.
	dgs, _, _ = plan(targets[:1], nil, LimitedBroadcast)
	if len(dgs) != 1 || dgs[0].src.Prefix.IsValid() || dgs[0].dst != LimitedBroadcast {
		t.Fatalf("unbound plan = %+v", dgs)
	}
	if _, _, err := plan([]Target{{MAC: "junk"}}, locals, LimitedBroadcast); err == nil {
		t.Fatal("plan without a valid MAC succeeded")
	}
}

func TestSendLoopback(t *testing.T) {
	directed, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer directed.Close()
	port := directed.LocalAddr().(*net.UDPAddr).Port
	limited, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.2", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("cannot bind 127.0.0.2 on this host: %v", err)
	}
	defer limited.Close()

	mac := "a4:bb:6d:12:34:56"
	s := &Sender{
		Ports: []int{port},
		Gap:   10 * time.Millisecond,
		Locals: func() ([]Local, error) {
			return []Local{{Interface: "lo", Prefix: netip.MustParsePrefix("127.0.0.1/8")}}, nil
		},
		Limited: netip.MustParseAddr("127.0.0.2"),
	}
	// The "broadcast" address of the stored subnet points at the loopback listener.
	targets := []Target{{MAC: mac, Subnets: []Subnet{{Prefix: netip.MustParsePrefix("127.0.0.9/8"), Broadcast: netip.MustParseAddr("127.0.0.1")}}}}
	rep, err := s.Send(context.Background(), targets)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sent != 6 || rep.Failed != 0 || rep.NoSharedSubnet || len(rep.Routes) != 2 {
		t.Fatalf("report = %+v, want 6 sent over 2 routes", rep)
	}
	if !slices.Equal(rep.MACs, []string{"a4:bb:6d:xx:xx:56"}) {
		t.Fatalf("macs = %v", rep.MACs)
	}
	want, _ := MagicPacket(mustMAC(mac))
	for name, c := range map[string]net.PacketConn{"directed": directed, "limited": limited} {
		for i := range 3 {
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			buf := make([]byte, 200)
			n, _, err := c.ReadFrom(buf)
			if err != nil {
				t.Fatalf("%s datagram %d: %v", name, i, err)
			}
			if !bytes.Equal(buf[:n], want) {
				t.Fatalf("%s datagram %d = % x", name, i, buf[:n])
			}
		}
		c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, _, err := c.ReadFrom(make([]byte, 200)); err == nil {
			t.Fatalf("%s listener got more than 3 datagrams", name)
		}
	}
}

func TestMonitorParser(t *testing.T) {
	var p monitorParser
	lines := []string{
		"/org/freedesktop/login1: org.freedesktop.login1.Manager.PrepareForShutdown (false,)",
		"/org/freedesktop/login1: org.freedesktop.login1.Manager.PrepareForSleep (true,)",
		"/org/freedesktop/login1: org.freedesktop.login1.Manager.PrepareForSleep (false,)",
		"signal time=1700000000.1 sender=:1.5 -> destination=(null destination) serial=9 path=/org/freedesktop/login1; interface=org.freedesktop.login1.Manager; member=PrepareForSleep",
		"   boolean true",
		"signal time=1700000001.1 sender=:1.5 -> destination=(null destination) serial=10 path=/org/freedesktop/login1; interface=org.freedesktop.login1.Manager; member=PrepareForSleep",
		"   boolean false",
		"   boolean true", // not after a header
	}
	var got []PowerEvent
	for _, l := range lines {
		if ev, ok := p.feed(l); ok {
			got = append(got, ev)
		}
	}
	if want := []PowerEvent{Suspending, Resumed, Suspending, Resumed}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}
