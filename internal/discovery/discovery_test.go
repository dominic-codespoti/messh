package discovery

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

func loopbackInterface(t *testing.T) string {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list interfaces: %v", err)
	}
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagLoopback != 0 && ifi.Flags&net.FlagUp != 0 {
			if _, ok := interfaceIPv4(&ifi); ok {
				return ifi.Name
			}
		}
	}
	t.Skip("no up loopback interface with an IPv4 address")
	return ""
}

// freeUDPPort picks an unused high port by binding loopback only.
func freeUDPPort(t *testing.T) uint16 {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	defer c.Close()
	return uint16(c.LocalAddr().(*net.UDPAddr).Port)
}

func TestTwoNodesSeeEachOtherOnLoopback(t *testing.T) {
	lo := loopbackInterface(t)
	group := netip.AddrPortFrom(netip.MustParseAddr("239.255.75.19"), freeUDPPort(t))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	nodes := []Announcement{
		{ID: strings.Repeat("a", 52), Name: "node-a", Port: 40001},
		{ID: strings.Repeat("b", 51) + "7", Name: "node-b", Port: 40002},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		self int
		s    Sighting
	}
	sightings := make(chan result, 64)
	errs := make(chan error, len(nodes))
	for i, self := range nodes {
		cfg := Config{
			Group:      group,
			Self:       self,
			Interval:   200 * time.Millisecond,
			Interfaces: []string{lo},
			Logger:     logger.With("node", self.Name),
			OnSighting: func(s Sighting) {
				select {
				case sightings <- result{i, s}:
				default:
				}
			},
		}
		go func() { errs <- Run(ctx, cfg) }()
	}

	seen := make([]bool, len(nodes))
	deadline := time.After(3 * time.Second)
	for !seen[0] || !seen[1] {
		select {
		case err := <-errs:
			skipIfUnsupported(t, err)
			t.Fatalf("Run returned early: %v", err)
		case r := <-sightings:
			peer := nodes[1-r.self]
			if r.s.ID != peer.ID {
				t.Fatalf("node %d saw unexpected ID %q (want %q)", r.self, r.s.ID, peer.ID)
			}
			if r.s.Name != peer.Name || r.s.V != 1 {
				t.Fatalf("node %d: bad announcement %+v", r.self, r.s.Announcement)
			}
			if int(r.s.Addr.Port()) != peer.Port {
				t.Fatalf("node %d: Addr port %d, want %d", r.self, r.s.Addr.Port(), peer.Port)
			}
			if !r.s.Addr.Addr().IsLoopback() {
				t.Fatalf("node %d: Addr IP %v is not loopback", r.self, r.s.Addr.Addr())
			}
			seen[r.self] = true
		case <-deadline:
			t.Fatalf("timed out; sightings received: %v", seen)
		}
	}

	cancel()
	for range nodes {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("Run after cancel: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
}

// skipIfUnsupported skips when the OS rejects multicast on the loopback interface.
func skipIfUnsupported(t *testing.T, err error) {
	t.Helper()
	var opErr *net.OpError
	var sysErr *os.SyscallError
	if err != nil && (errors.As(err, &opErr) || errors.As(err, &sysErr)) && strings.Contains(err.Error(), "join") {
		t.Skipf("multicast on loopback unsupported on this OS: %v", err)
	}
}

func TestValidate(t *testing.T) {
	ok := Announcement{V: 1, ID: strings.Repeat("z", 52), Name: "n", Port: 1}
	cases := []struct {
		name  string
		mod   func(*Announcement)
		valid bool
	}{
		{"minimal", func(*Announcement) {}, true},
		{"name 64 bytes, port 65535", func(a *Announcement) { a.Name = strings.Repeat("x", 64); a.Port = 65535 }, true},
		{"version 2", func(a *Announcement) { a.V = 2 }, false},
		{"id 51 chars", func(a *Announcement) { a.ID = a.ID[:51] }, false},
		{"id 53 chars", func(a *Announcement) { a.ID += "a" }, false},
		{"id uppercase", func(a *Announcement) { a.ID = "A" + a.ID[1:] }, false},
		{"id digit 8", func(a *Announcement) { a.ID = "8" + a.ID[1:] }, false},
		{"empty name", func(a *Announcement) { a.Name = "" }, false},
		{"name 65 bytes", func(a *Announcement) { a.Name = strings.Repeat("x", 65) }, false},
		{"port 0", func(a *Announcement) { a.Port = 0 }, false},
		{"port 65536", func(a *Announcement) { a.Port = 65536 }, false},
	}
	for _, c := range cases {
		a := ok
		c.mod(&a)
		if err := validate(a); (err == nil) != c.valid {
			t.Errorf("%s: validate = %v, want valid=%v", c.name, err, c.valid)
		}
	}
}

func TestAutoCandidatesFilter(t *testing.T) {
	up := net.FlagUp | net.FlagMulticast
	all := []net.Interface{
		{Index: 1, Name: "lo", Flags: up | net.FlagLoopback},
		{Index: 2, Name: "eth0", Flags: up},
		{Index: 3, Name: "down0", Flags: net.FlagMulticast},
		{Index: 4, Name: "nomcast0", Flags: net.FlagUp},
		{Index: 5, Name: "v6only0", Flags: up},
		{Index: 6, Name: "wlan0", Flags: up},
	}
	addrs := map[string]netip.Addr{
		"lo":       netip.MustParseAddr("127.0.0.1"),
		"eth0":     netip.MustParseAddr("192.168.1.2"),
		"down0":    netip.MustParseAddr("10.0.0.1"),
		"nomcast0": netip.MustParseAddr("10.0.0.2"),
		"wlan0":    netip.MustParseAddr("192.168.1.3"),
	}
	got := autoCandidates(all, func(ifi *net.Interface) (netip.Addr, bool) {
		a, ok := addrs[ifi.Name]
		return a, ok
	})
	var names []string
	for _, c := range got {
		if c.ip != addrs[c.ifi.Name] {
			t.Errorf("%s: ip %v, want %v", c.ifi.Name, c.ip, addrs[c.ifi.Name])
		}
		names = append(names, c.ifi.Name)
	}
	if strings.Join(names, ",") != "eth0,wlan0" {
		t.Fatalf("candidates = %v, want [eth0 wlan0]", names)
	}
}

func TestDiffLinks(t *testing.T) {
	cand := func(idx int, name, ip string) candidate {
		return candidate{net.Interface{Index: idx, Name: name}, netip.MustParseAddr(ip)}
	}
	key := func(idx int, ip string) linkKey { return linkKey{idx, netip.MustParseAddr(ip)} }
	names := func(cs []candidate) string {
		var s []string
		for _, c := range cs {
			s = append(s, c.ifi.Name+"="+c.ip.String())
		}
		return strings.Join(s, ",")
	}

	cases := []struct {
		name string
		have map[string]linkKey
		want []candidate
		add  string
		drop string
	}{
		{"first scan joins all", nil,
			[]candidate{cand(2, "eth0", "192.168.1.2"), cand(3, "wlan0", "192.168.1.3")},
			"eth0=192.168.1.2,wlan0=192.168.1.3", ""},
		{"unchanged is a no-op",
			map[string]linkKey{"eth0": key(2, "192.168.1.2")},
			[]candidate{cand(2, "eth0", "192.168.1.2")},
			"", ""},
		{"all gone drops all",
			map[string]linkKey{"eth0": key(2, "192.168.1.2"), "wlan0": key(3, "192.168.1.3")},
			nil,
			"", "eth0,wlan0"},
		{"address change rebuilds, new joins, vanished drops",
			map[string]linkKey{"eth0": key(2, "192.168.1.2"), "wlan0": key(3, "192.168.1.3"), "usb0": key(4, "10.0.0.1")},
			[]candidate{cand(2, "eth0", "192.168.1.2"), cand(3, "wlan0", "10.1.1.9"), cand(5, "tap0", "172.16.0.1")},
			"wlan0=10.1.1.9,tap0=172.16.0.1", "usb0,wlan0"},
		{"index change with same address rebuilds",
			map[string]linkKey{"wlan0": key(3, "192.168.1.3")},
			[]candidate{cand(7, "wlan0", "192.168.1.3")},
			"wlan0=192.168.1.3", "wlan0"},
	}
	for _, c := range cases {
		add, drop := diffLinks(c.have, c.want)
		if got := names(add); got != c.add {
			t.Errorf("%s: add = %q, want %q", c.name, got, c.add)
		}
		if got := strings.Join(drop, ","); got != c.drop {
			t.Errorf("%s: drop = %q, want %q", c.name, got, c.drop)
		}
	}
}

func TestSleepSignalFlagsAnnouncements(t *testing.T) {
	lo := loopbackInterface(t)
	group := netip.AddrPortFrom(netip.MustParseAddr("239.255.75.19"), freeUDPPort(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sleep := make(chan bool, 1)
	sleeper := Config{
		Group:      group,
		Self:       Announcement{ID: strings.Repeat("c", 52), Name: "laptop", Port: 40003},
		Interval:   200 * time.Millisecond,
		Interfaces: []string{lo},
		Sleep:      sleep,
		Logger:     slog.New(slog.DiscardHandler),
	}
	got := make(chan bool, 64)
	listener := Config{
		Group:      group,
		Self:       Announcement{ID: strings.Repeat("d", 52), Name: "raspi", Port: 40004},
		Interval:   time.Hour,
		Interfaces: []string{lo},
		Logger:     slog.New(slog.DiscardHandler),
		OnSighting: func(s Sighting) {
			select {
			case got <- s.Sleeping:
			default:
			}
		},
	}
	errs := make(chan error, 2)
	go func() { errs <- Run(ctx, sleeper) }()
	go func() { errs <- Run(ctx, listener) }()

	waitFlag := func(want bool) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case err := <-errs:
				skipIfUnsupported(t, err)
				t.Fatalf("Run returned early: %v", err)
			case s := <-got:
				if s == want {
					return
				}
			case <-deadline:
				t.Fatalf("no announcement with sleeping=%v", want)
			}
		}
	}
	waitFlag(false)
	sleep <- true
	waitFlag(true)
	sleep <- false
	waitFlag(false)
}
