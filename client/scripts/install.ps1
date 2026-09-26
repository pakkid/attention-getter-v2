# Installs or updates the Attention Getter client on Windows.
#
#   irm https://raw.githubusercontent.com/pakkid/attention-getter-v2/main/client/scripts/install.ps1 | iex
#
# Downloads the latest release (or $env:AG_VERSION, e.g. "v0.1.1"), checks it against
# SHA256SUMS, replaces any running copy, runs `setup` if this PC isn't paired yet, and
# enables start at login. Re-run it any time to update; the pairing and settings are kept.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # the progress bar makes iwr very slow on PowerShell 5.1
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$repo = 'pakkid/attention-getter-v2'
$asset = 'attention-getter-windows-x86_64.exe'
$base = if ($env:AG_VERSION) { "https://github.com/$repo/releases/download/$env:AG_VERSION" } else { "https://github.com/$repo/releases/latest/download" }
$dir = Join-Path $env:LOCALAPPDATA 'Programs'
$exe = Join-Path $dir 'attention-getter.exe'
$config = Join-Path $env:APPDATA 'attention-getter\config\config.toml'

$tmp = Join-Path ([IO.Path]::GetTempPath()) "attention-getter-$([guid]::NewGuid()).exe"
try {
    Write-Host "Downloading $base/$asset"
    Invoke-WebRequest "$base/$asset" -OutFile $tmp -UseBasicParsing
    $sums = (Invoke-WebRequest "$base/SHA256SUMS" -UseBasicParsing).Content
    if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
    $line = ($sums -split "`n") | Where-Object { $_ -match "\s\*?$([regex]::Escape($asset))\s*$" } | Select-Object -First 1
    if (-not $line) { throw "SHA256SUMS has no entry for $asset" }
    $want = ($line -split '\s+')[0].ToLower()
    $got = (Get-FileHash $tmp -Algorithm SHA256).Hash.ToLower()
    if ($got -ne $want) { throw "Checksum mismatch: expected $want, got $got" }
    Write-Host "Checksum OK"

    # Replace an existing install wherever it lives: the autostart entry says which exe runs
    # at login. The service and any open popup are that same exe, so stop every copy.
    $old = $null
    $run = (Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -ErrorAction SilentlyContinue).AttentionGetter
    if ($run -match '^"([^"]+)"') { $old = $Matches[1] }
    $running = Get-Process -ErrorAction SilentlyContinue | Where-Object {
        $_.Name -like 'attention-getter*' -or ($old -and $_.Path -eq $old)
    }
    if ($running) {
        Write-Host "Stopping the running client ($(@($running).Count) process(es))"
        $running | Stop-Process -Force
        $running | Wait-Process -Timeout 10 -ErrorAction SilentlyContinue
    }
    New-Item -ItemType Directory -Force $dir | Out-Null
    Move-Item -Force $tmp $exe
    Unblock-File $exe
    if ($old -and $old -ne $exe -and (Test-Path $old)) {
        Remove-Item -Force $old -ErrorAction SilentlyContinue
        Write-Host "Removed the previous copy at $old"
    }
    Write-Host "Installed $exe"
} finally {
    Remove-Item $tmp -ErrorAction SilentlyContinue
}

# The exe is a GUI-subsystem program, so PowerShell won't wait for it unless told to.
if (-not (Test-Path $config)) {
    Write-Host "Not paired yet: starting setup in a new window..."
    Start-Process $exe -ArgumentList 'setup' -Wait
    if (-not (Test-Path $config)) { throw "Setup didn't finish. Run '$exe setup', then run this installer again." }
}
Start-Process $exe -ArgumentList 'install' -NoNewWindow -Wait
Write-Host "Done. Preview the popup with: & '$exe' test"
