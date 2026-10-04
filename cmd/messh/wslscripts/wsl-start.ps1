# wsl-start.ps1: owner-context WSL boot for one messh WSL target.
# Runs as the owner at logon from the unprivileged startup task. Launches
# the selected distro for the owner user and starts its persistent user
# service. Never touches host routing, firewall, or the Windows node.
# Placeholders DISTRO/USER/GUESTSTATE and port defaults are filled by
# `messh wsl setup` at install time; this copy lives in the owner's state
# directory and carries no secret.
$ErrorActionPreference = 'Stop'

$distro = '@@DISTRO@@'
$guestUser = '@@USER@@'
$guestState = '@@GUESTSTATE@@'
$meshPort = @@MESHPORT@@
$localPort = @@LOCALPORT@@

$wsl = Join-Path $env:SystemRoot 'System32\wsl.exe'
$logDir = Join-Path $env:LOCALAPPDATA 'messh\wsl-logs'
[void][System.IO.Directory]::CreateDirectory($logDir)
$log = Join-Path $logDir 'wsl-start.log'

function Log([string]$msg) {
  ('[{0:u}] {1}' -f [DateTime]::UtcNow, $msg) | Out-File -LiteralPath $log -Append -Encoding utf8
}

try {
  & $wsl -d $distro -u $guestUser -- /bin/sh -c 'systemctl --user start messh.service' 2>&1 | Out-File -LiteralPath $log -Append -Encoding utf8
  if ($LASTEXITCODE -ne 0) { throw "guest service start failed for $guestUser@$distro (state $guestState mesh $meshPort local $localPort)" }
  Log("started messh.service for $guestUser@$distro (mesh $meshPort, guest local $localPort unexposed)")
} catch {
  Log("FAILED: $_")
  throw
}
