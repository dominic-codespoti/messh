//go:build windows

package main

import (
	"strings"
	"testing"

	"messh/internal/state"
)

func testWSLSetupTarget() state.WSLTargetConfig {
	return state.WSLTargetConfig{
		Distro:             "Ubuntu",
		User:               "dom",
		Name:               "dompc-wsl",
		ID:                 strings.Repeat("c", 52),
		GuestState:         "/home/dom/.local/state/messh",
		HostAddress:        "192.168.1.31",
		TaskName:           state.DefaultWSLTaskName,
		RouteTaskName:      state.DefaultWSLRouteTaskName,
		MeshPort:           7521,
		LocalPort:          7522,
		HostInterfaceIndex: 10,
		AllowedPeers:       []string{"192.168.1.189"},
	}
}

func TestClassifyWSLPortproxyExactRowIsFine(t *testing.T) {
	cfg := testWSLSetupTarget()
	netsh := "Listen Address :  ignored header\n" +
		"192.168.1.31  7521  172.25.172.58  7521\n"
	if clash := classifyWSLPortproxy(netsh, cfg); clash != "" {
		t.Fatalf("guest-IP row reported as collision: %s", clash)
	}
}

func TestClassifyWSLPortproxyStaleGuestIP(t *testing.T) {
	cfg := testWSLSetupTarget()
	netsh := "172.25.172.58  7521  172.25.172.58  7521\n"
	// Different listen address: not our row, no collision verdict here.
	if clash := classifyWSLPortproxy(netsh, cfg); clash != "" {
		t.Fatalf("other-listen row reported as collision: %s", clash)
	}
	netsh = "192.168.1.31  7521  127.0.0.1  7521\n"
	if clash := classifyWSLPortproxy(netsh, cfg); clash == "" {
		t.Fatalf("loopback row accepted as a replaceable stale guest address")
	}
}

func TestClassifyWSLPortproxyUnrelatedCollision(t *testing.T) {
	cfg := testWSLSetupTarget()
	for _, netsh := range []string{
		"192.168.1.31  7521  127.0.0.1  9999\n",
		"192.168.1.31  7521  192.168.1.31  7521\n",
	} {
		if clash := classifyWSLPortproxy(netsh, cfg); clash == "" {
			t.Fatalf("unrelated row accepted: %q", netsh)
		}
	}
}

func TestClassifyWSLPortproxyOtherPortsIgnored(t *testing.T) {
	cfg := testWSLSetupTarget()
	netsh := "192.168.1.31  7519  127.0.0.1  7519\n" +
		"192.168.1.31  7522  10.0.0.9  7522\n"
	if clash := classifyWSLPortproxy(netsh, cfg); clash != "" {
		t.Fatalf("other ports reported as collision: %s", clash)
	}
}

func TestWSLDecodeConsoleSchtasksXML(t *testing.T) {
	xml := "<Task><Actions><Exec><Command>powershell</Command></Exec></Actions><Comment>messh wsl wsl-start.ps1</Comment></Task>"
	var utf16 []byte
	utf16 = append(utf16, 0xFF, 0xFE)
	for _, r := range xml {
		utf16 = append(utf16, byte(r), 0)
	}
	got := wslDecodeConsole(utf16)
	if !strings.Contains(got, "messh wsl") || !strings.Contains(got, "wsl-start.ps1") {
		t.Fatalf("schtasks XML did not decode: %q", got)
	}
}

func TestParseWSLInventoryUTF16(t *testing.T) {
	utf16 := func(s string) []byte {
		var b []byte
		b = append(b, 0xFF, 0xFE)
		for _, r := range s {
			b = append(b, byte(r), 0)
			if r > 0xFF {
				b[len(b)-1] = byte(r >> 8)
			}
		}
		return b
	}
	got := parseWSLInventory(utf16("Ubuntu\r\ndocker-desktop\r\n\r\n"))
	if len(got) != 2 || got[0] != "Ubuntu" || got[1] != "docker-desktop" {
		t.Fatalf("UTF-16 inventory = %q", got)
	}
	got = parseWSLInventory([]byte("Ubuntu\n\nDebian\n"))
	if len(got) != 2 || got[0] != "Ubuntu" || got[1] != "Debian" {
		t.Fatalf("UTF-8 inventory = %q", got)
	}
	// NUL without BOM is still UTF-16LE, not a truncated UTF-8 line.
	got = parseWSLInventory([]byte{'U', 0, 'b', 0, '\n', 0})
	if len(got) != 1 || got[0] != "Ub" {
		t.Fatalf("bare-NUL inventory = %q", got)
	}
}
