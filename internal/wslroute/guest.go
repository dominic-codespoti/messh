// Package wslroute contains the shared Windows WSL NAT guest resolver.
package wslroute

// GuestAddressScript contains PowerShell function definitions only. Callers
// embed these definitions into the secured route script or invoke the resolver.
const GuestAddressScript = `
function Test-MesshGuestIPv4([string]$Address) {
  $parsed = $null
  if (-not [Net.IPAddress]::TryParse($Address, [ref]$parsed) -or $parsed.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) { return $false }
  $b = $parsed.GetAddressBytes()
  if ($b[3] -eq 0 -or $b[3] -eq 255) { return $false }
  return ($b[0] -eq 10) -or ($b[0] -eq 172 -and $b[1] -ge 16 -and $b[1] -le 31) -or ($b[0] -eq 192 -and $b[1] -eq 168)
}

function Test-MesshGuestMac([string]$Mac) {
  if ($Mac -notmatch '^(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}$') { return $false }
  $compact = $Mac -replace '[:-]', ''
  if ($compact -match '^0{12}$' -or $compact -match '^f{12}$') { return $false }
  $first = [Convert]::ToInt32($compact.Substring(0, 2), 16)
  return (($first -band 1) -eq 0)
}

function Get-MesshWSLGuestAddress {
  $Adapters = @(Get-NetAdapter -IncludeHidden -ErrorAction Stop)
  $wslAdapters = @($Adapters | Where-Object { $_.Status -eq 'Up' -and $_.InterfaceAlias -in @('vEthernet (WSL)', 'vEthernet (WSL (Hyper-V firewall))') })
  if ($wslAdapters.Count -ne 1) { throw "expected exactly one Up default WSL2 NAT adapter; found $($wslAdapters.Count)" }
  $adapter = $wslAdapters[0]
  $Neighbors = @(Get-NetNeighbor -InterfaceIndex $adapter.InterfaceIndex -AddressFamily IPv4 -ErrorAction Stop)
  $eligible = @('Reachable', 'Stale', 'Delay', 'Probe')
  $addresses = @{}
  foreach ($neighbor in $Neighbors) {
    if ([int]$neighbor.InterfaceIndex -ne [int]$adapter.InterfaceIndex) { continue }
    if ([string]$neighbor.State -notin $eligible) { continue }
    if (-not (Test-MesshGuestMac ([string]$neighbor.LinkLayerAddress))) { continue }
    $parsed = $null
    if (-not [Net.IPAddress]::TryParse([string]$neighbor.IPAddress, [ref]$parsed) -or $parsed.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) { continue }
    $address = $parsed.ToString()
    if (Test-MesshGuestIPv4 $address) { $addresses[$address] = $true }
  }
  if ($addresses.Count -ne 1) { throw "expected exactly one eligible WSL NAT neighbor; found $($addresses.Count)" }
  return [string](@($addresses.Keys)[0])
}
`
