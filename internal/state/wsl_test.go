package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func testWSLTarget() WSLTargetConfig {
	return WSLTargetConfig{
		Distro:             "Ubuntu",
		User:               "dom",
		Name:               "dompc-wsl",
		ID:                 strings.Repeat("a", 51) + "2",
		GuestState:         "/home/dom/.local/state/messh",
		HostAddress:        "192.168.1.31",
		GuestAddress:       "172.25.172.58",
		TaskName:           DefaultWSLTaskName,
		RouteTaskName:      DefaultWSLRouteTaskName,
		MeshPort:           7521,
		LocalPort:          7522,
		HostInterfaceIndex: 10,
		AllowedPeers:       []string{"192.168.1.189"},
	}
}

func TestWSLTargetRoundTrip(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	if got, err := p.LoadWSLTarget(); err != nil || got != nil {
		t.Fatalf("unconfigured load = %+v, %v; want nil, nil", got, err)
	}
	want := testWSLTarget()
	if err := p.SaveWSLTarget(want); err != nil {
		t.Fatal(err)
	}
	got, err := p.LoadWSLTarget()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("round trip = %+v, want %+v", *got, want)
	}
	// GuestAddress is optional information: empty must also round-trip.
	want.GuestAddress = ""
	if err := p.SaveWSLTarget(want); err != nil {
		t.Fatal(err)
	}
	got, err = p.LoadWSLTarget()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("round trip without guest address = %+v, want %+v", *got, want)
	}
}

func TestWSLTargetSaveRefusesInvalidWithoutTouchingDisk(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	bad := testWSLTarget()
	bad.MeshPort = 0
	if err := p.SaveWSLTarget(bad); err == nil {
		t.Fatal("SaveWSLTarget accepted mesh-port 0")
	}
	if _, err := os.Stat(filepath.Join(p.Root, "wsl-target.json")); !os.IsNotExist(err) {
		t.Fatalf("invalid save left a target file: %v", err)
	}
}

func TestWSLTargetLoadCorrupt(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Root, "wsl-target.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := p.LoadWSLTarget(); err == nil || got != nil {
		t.Fatalf("corrupt load = %+v, %v; want an error", got, err)
	}
}

func TestWSLTargetValidate(t *testing.T) {
	if err := testWSLTarget().Validate(); err != nil {
		t.Fatalf("valid target rejected: %v", err)
	}
	cases := map[string]func(*WSLTargetConfig){
		"empty distro":            func(c *WSLTargetConfig) { c.Distro = "" },
		"distro whitespace":       func(c *WSLTargetConfig) { c.Distro = "Ubun tu" },
		"distro quote":            func(c *WSLTargetConfig) { c.Distro = `Ub"untu` },
		"bad user":                func(c *WSLTargetConfig) { c.User = "Dom" },
		"empty user":              func(c *WSLTargetConfig) { c.User = "" },
		"empty name":              func(c *WSLTargetConfig) { c.Name = "" },
		"short id":                func(c *WSLTargetConfig) { c.ID = "abc" },
		"id bad charset":          func(c *WSLTargetConfig) { c.ID = strings.Repeat("!", 52) },
		"guest mnt share":         func(c *WSLTargetConfig) { c.GuestState = "/mnt/c/Users/dom/state" },
		"guest windows path":      func(c *WSLTargetConfig) { c.GuestState = `C:\Users\dom\state` },
		"guest relative":          func(c *WSLTargetConfig) { c.GuestState = "home/dom/state" },
		"guest trailing slash":    func(c *WSLTargetConfig) { c.GuestState = "/home/dom/state/" },
		"guest root":              func(c *WSLTargetConfig) { c.GuestState = "/" },
		"host name":               func(c *WSLTargetConfig) { c.HostAddress = "desktop.local" },
		"host cidr":               func(c *WSLTargetConfig) { c.HostAddress = "192.168.1.31/24" },
		"host loopback":           func(c *WSLTargetConfig) { c.HostAddress = "127.0.0.1" },
		"host public":             func(c *WSLTargetConfig) { c.HostAddress = "8.8.8.8" },
		"host link local":         func(c *WSLTargetConfig) { c.HostAddress = "169.254.3.4" },
		"guest address zone":      func(c *WSLTargetConfig) { c.GuestAddress = "fe80::1%eth0" },
		"guest address unspecified": func(c *WSLTargetConfig) { c.GuestAddress = "0.0.0.0" },
		"bad task name":           func(c *WSLTargetConfig) { c.TaskName = "messh wsl" },
		"bad route task name":     func(c *WSLTargetConfig) { c.RouteTaskName = "../route" },
		"equal task names":        func(c *WSLTargetConfig) { c.RouteTaskName = c.TaskName },
		"mesh port zero":          func(c *WSLTargetConfig) { c.MeshPort = 0 },
		"mesh port too big":       func(c *WSLTargetConfig) { c.MeshPort = 65536 },
		"local port zero":         func(c *WSLTargetConfig) { c.LocalPort = 0 },
		"ports equal":             func(c *WSLTargetConfig) { c.LocalPort = c.MeshPort },
		"interface zero":          func(c *WSLTargetConfig) { c.HostInterfaceIndex = 0 },
		"interface negative":      func(c *WSLTargetConfig) { c.HostInterfaceIndex = -3 },
		"no peers":                func(c *WSLTargetConfig) { c.AllowedPeers = nil },
		"peer public":             func(c *WSLTargetConfig) { c.AllowedPeers = []string{"8.8.8.8"} },
		"peer cidr":               func(c *WSLTargetConfig) { c.AllowedPeers = []string{"192.168.1.0/24"} },
		"peer loopback":           func(c *WSLTargetConfig) { c.AllowedPeers = []string{"127.0.0.1"} },
		"peer is host":            func(c *WSLTargetConfig) { c.AllowedPeers = []string{"192.168.1.31"} },
		"peer duplicate":          func(c *WSLTargetConfig) { c.AllowedPeers = []string{"192.168.1.189", "192.168.1.189"} },
		"peer broad cgnat":        func(c *WSLTargetConfig) { c.AllowedPeers = []string{"100.64.0.1"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := testWSLTarget()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("Validate accepted %s: %+v", name, c)
			}
		})
	}
}
func TestWSLTargetTooManyPeers(t *testing.T) {
	c := testWSLTarget()
	for i := 2; i <= 17; i++ {
		c.AllowedPeers = append(c.AllowedPeers, "10.0.0."+strconv.Itoa(i))
	}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted 17 allowed peers")
	}
}

func TestWSLFirewallRuleName(t *testing.T) {
	if got := WSLFirewallRuleName(7521); got != "messh wsl (mesh TCP 7521)" {
		t.Fatalf("rule name = %q", got)
	}
}
