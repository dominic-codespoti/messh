package wslroute

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestGuestAddressScriptSemantics(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell runtime not installed")
	}
	cases := []struct {
		name, adapters, neighbors, want string
		wantError                       bool
	}{
		{"ignores global ARP and unrelated adapter", "@(@{InterfaceIndex=8;InterfaceAlias='Wi-Fi';Status='Up'},@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@(@{InterfaceIndex=8;IPAddress='172.20.1.2';LinkLayerAddress='00-11-22-33-44-55';State='Reachable'},@{InterfaceIndex=12;IPAddress='172.30.8.5';LinkLayerAddress='00-11-22-33-44-56';State='Stale'})", "172.30.8.5", false},
		{"no adapter", "@(@{InterfaceIndex=8;InterfaceAlias='Ethernet';Status='Up'})", "@()", "", true},
		{"ambiguous adapters", "@(@{InterfaceIndex=8;InterfaceAlias='vEthernet (WSL)';Status='Up'},@{InterfaceIndex=9;InterfaceAlias='vEthernet (WSL (Hyper-V firewall))';Status='Up'})", "@()", "", true},
		{"no neighbors", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@()", "", true},
		{"no eligible neighbor state", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@(@{InterfaceIndex=12;IPAddress='172.20.1.2';LinkLayerAddress='00-11-22-33-44-55';State='Unreachable'})", "", true},
		{"ambiguous neighbor addresses", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@(@{InterfaceIndex=12;IPAddress='172.20.1.2';LinkLayerAddress='00-11-22-33-44-55';State='Reachable'},@{InterfaceIndex=12;IPAddress='10.20.1.2';LinkLayerAddress='00-11-22-33-44-56';State='Probe'})", "", true},
		{"invalid MAC and subnet broadcast are ignored", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@(@{InterfaceIndex=12;IPAddress='172.20.1.2';LinkLayerAddress='00-00-00-00-00-00';State='Reachable'},@{InterfaceIndex=12;IPAddress='10.4.5.3';LinkLayerAddress='01-11-22-33-44-55';State='Reachable'},@{InterfaceIndex=12;IPAddress='192.168.1.255';LinkLayerAddress='00-11-22-33-44-55';State='Stale'})", "", true},
		{"Reachable private 10 range", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@(@{InterfaceIndex=12;IPAddress='10.4.5.6';LinkLayerAddress='00-11-22-33-44-55';State='Reachable'})", "10.4.5.6", false},
		{"Stale private 172 range", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@(@{InterfaceIndex=12;IPAddress='172.16.2.3';LinkLayerAddress='00-11-22-33-44-56';State='Stale'})", "172.16.2.3", false},
		{"Delay private 192 range", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL (Hyper-V firewall))';Status='Up'})", "@(@{InterfaceIndex=12;IPAddress='192.168.1.3';LinkLayerAddress='00-11-22-33-44-57';State='Delay'})", "192.168.1.3", false},
		{"Probe private 172 range", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@(@{InterfaceIndex=12;IPAddress='172.31.2.3';LinkLayerAddress='00-11-22-33-44-58';State='Probe'})", "172.31.2.3", false},
		{"deduplicates repeated neighbor address", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@(@{InterfaceIndex=12;IPAddress='10.4.5.6';LinkLayerAddress='00-11-22-33-44-55';State='Reachable'},@{InterfaceIndex=12;IPAddress='10.4.5.6';LinkLayerAddress='00-11-22-33-44-56';State='Stale'})", "10.4.5.6", false},
		{"interface index scoping", "@(@{InterfaceIndex=12;InterfaceAlias='vEthernet (WSL)';Status='Up'})", "@(@{InterfaceIndex=8;IPAddress='172.20.1.2';LinkLayerAddress='00-11-22-33-44-55';State='Reachable'})", "", true},
	}
	fixtureFunctions := `
function Get-NetAdapter { param([switch]$IncludeHidden, [string]$ErrorAction); if (-not $IncludeHidden -or $ErrorAction -ne 'Stop') { throw 'unexpected adapter query args' }; return $script:fixtureAdapters }
function Get-NetNeighbor { param([int]$InterfaceIndex, [string]$AddressFamily, [string]$ErrorAction); if ($InterfaceIndex -ne 12 -or $AddressFamily -ne 'IPv4' -or $ErrorAction -ne 'Stop') { throw 'unexpected neighbor query args' }; $script:neighborQueryChecked = $true; return $script:fixtureNeighbors }
`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command := GuestAddressScript + fixtureFunctions + "\n$script:fixtureAdapters=" + tc.adapters + "\n$script:fixtureNeighbors=" + tc.neighbors + "\ntry { $actual=Get-MesshWSLGuestAddress; if (-not $script:neighborQueryChecked) { exit 8 }; if ($actual -ne " + quotePS(tc.want) + ") { [Console]::Error.WriteLine(\"got $actual\"); exit 9 }; exit 0 } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 7 }"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected resolver error, output %q", out)
				}
				if ctx.Err() != nil {
					t.Fatalf("resolver timed out: %v", ctx.Err())
				}
				return
			}
			if err != nil {
				t.Fatalf("resolver failed: %v: %s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != "" {
				t.Fatalf("unexpected PowerShell output %q", got)
			}
		})
	}
}

func quotePS(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
