# wsl-route.ps1: SYSTEM route-only refresh for one messh WSL target.
# Runs as SYSTEM from the secured route task or from the elevated installer.
# Reads only its administrator-owned sibling route.json. Applies host-native
# networking: one netsh v4tov4 portproxy row (LAN mesh port to the live guest
# IP:same port) and one narrow inbound firewall rule (TCP mesh port from
# explicit peers on Private). Never runs wsl.exe and never reads user-writable
# state. Resolves guest through the default WSL2 NAT adapter neighbor table.
$ErrorActionPreference = 'Stop'

function Fail([string]$msg) { throw $msg }

$dir = Split-Path -Parent $MyInvocation.MyCommand.Path
$cfgPath = Join-Path $dir 'route.json'
$netsh = Join-Path $env:SystemRoot 'System32\netsh.exe'

$cfgItem = Get-Item -LiteralPath $cfgPath -Force -ErrorAction Stop
if (($cfgItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
  Fail('refusing reparse config; re-run setup elevated')
}
$ownerLines = & icacls $cfgPath 2>&1
if ($LASTEXITCODE -ne 0) { Fail('cannot read config ownership') }
$ownerOk = $false
foreach ($line in @($ownerLines)) {
  if ($line -match 'BUILTIN\\Administrators|NT AUTHORITY\\SYSTEM') { $ownerOk = $true }
}
if (-not $ownerOk) { Fail('refusing config without Administrators/SYSTEM ownership; re-run setup elevated') }

$cfg = Get-Content -LiteralPath $cfgPath -Raw -ErrorAction Stop | ConvertFrom-Json
$hostAddr = [string]$cfg.host_address
$meshPort = [int]$cfg.mesh_port
$ruleName = [string]$cfg.rule_name
$peers = @($cfg.allowed_peers | ForEach-Object { [string]$_ })
$portText = [string]$meshPort

$ip = $null
if ([string]::IsNullOrWhiteSpace($hostAddr) -or -not [Net.IPAddress]::TryParse($hostAddr, [ref]$ip)) {
  Fail('config host_address is not a literal IP')
}
$b = $ip.GetAddressBytes()
$private = ($b[0] -eq 10) -or ($b[0] -eq 172 -and $b[1] -ge 16 -and $b[1] -le 31) -or ($b[0] -eq 192 -and $b[1] -eq 168)
if (-not $private) { Fail('config host_address is not private IPv4') }
if ($meshPort -lt 1 -or $meshPort -gt 65535) { Fail('config mesh_port out of range') }
if ([string]::IsNullOrWhiteSpace($ruleName) -or $ruleName.Length -gt 120) { Fail('config rule_name is missing') }
if ($peers.Count -lt 1 -or $peers.Count -gt 16) { Fail('config needs 1-16 allowed peers') }
foreach ($p in $peers) {
  $pa = $null
  if (-not [Net.IPAddress]::TryParse($p, [ref]$pa)) { Fail('config peer is not a literal IP') }
  $pb = $pa.GetAddressBytes()
  $pp = ($pb[0] -eq 10) -or ($pb[0] -eq 172 -and $pb[1] -ge 16 -and $pb[1] -le 31) -or ($pb[0] -eq 192 -and $pb[1] -eq 168)
  if (-not $pp -or $p -eq $hostAddr) { Fail('config peer is not an explicit private peer') }
}
$peersCsv = $peers -join ','

try { $guestIp = Get-MesshWSLGuestAddress } catch { Fail("cannot resolve default WSL2 NAT guest: $($_.Exception.Message)") }

& $netsh interface portproxy delete v4tov4 listenaddress=$hostAddr listenport=$portText | Out-Null
& $netsh interface portproxy add v4tov4 listenaddress=$hostAddr listenport=$portText connectaddress=$guestIp connectport=$portText
if ($LASTEXITCODE -ne 0) { Fail('netsh portproxy add failed') }

$existing = Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue
if ($null -ne $existing) {
  $pf = $existing | Get-NetFirewallPortFilter
  if ($pf.Protocol -ne 'TCP' -or ([string]$pf.LocalPort) -ne $portText) {
    Fail('unrelated firewall rule collides; remove it, then refresh elevated')
  }
  $af = $existing | Get-NetFirewallAddressFilter
  if (([string]$af.RemoteAddress) -ne $peersCsv) {
    Fail('firewall scope changed; remove it, then refresh elevated')
  }
  Set-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort $portText -RemoteAddress $peersCsv -Profile Private -Enabled True -ErrorAction Stop
} else {
  New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort $portText -RemoteAddress $peersCsv -Profile Private -Enabled True -ErrorAction Stop | Out-Null
}

$verify = & $netsh interface portproxy show v4tov4 2>&1
$found = $false
foreach ($line in @($verify)) {
  $f = @($line -split '\s+' | Where-Object { $_ -ne '' })
  if ($f.Count -ge 4 -and $f[0] -eq $hostAddr -and $f[1] -eq $portText -and $f[2] -eq $guestIp -and $f[3] -eq $portText) { $found = $true }
}
if (-not $found) { Fail('portproxy not present after apply') }
$rule = Get-NetFirewallRule -DisplayName $ruleName -ErrorAction Stop
'route ok'
