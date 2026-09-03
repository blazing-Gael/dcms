# DCMS installer for Windows. Downloads the prebuilt binary from the latest
# GitHub release and installs it — no Go toolchain required.
#
#   irm https://raw.githubusercontent.com/blazing-Gael/dcms/main/scripts/install.ps1 | iex
#
# Overrides (env vars):
#   DCMS_VERSION      a specific tag (e.g. v0.1.0-beta.1); default: latest
#   DCMS_INSTALL_DIR  where to put dcms.exe; default: %LOCALAPPDATA%\dcms\bin
$ErrorActionPreference = 'Stop'

$repo = 'blazing-Gael/dcms'

# ── Detect architecture ─────────────────────────────────────────────────────
$arch = if ([Environment]::Is64BitOperatingSystem) {
  if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
} else {
  throw 'unsupported architecture (32-bit)'
}
$asset = "dcms_windows_$arch.zip"

# ── Resolve URLs ────────────────────────────────────────────────────────────
$version = $env:DCMS_VERSION
if ([string]::IsNullOrEmpty($version) -or $version -eq 'latest') {
  $base = "https://github.com/$repo/releases/latest/download"
} else {
  $base = "https://github.com/$repo/releases/download/$version"
}
$url = "$base/$asset"
$sumsUrl = "$base/checksums.txt"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("dcms-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
  $zip = Join-Path $tmp $asset
  Write-Host "Downloading $asset..."
  Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing

  # ── Verify checksum when available ────────────────────────────────────────
  try {
    $sums = Join-Path $tmp 'checksums.txt'
    Invoke-WebRequest -Uri $sumsUrl -OutFile $sums -UseBasicParsing
    $line = Select-String -Path $sums -Pattern ([regex]::Escape($asset) + '$') | Select-Object -First 1
    if ($line) {
      $expected = ($line.Line -split '\s+')[0]
      $actual = (Get-FileHash -Path $zip -Algorithm SHA256).Hash.ToLower()
      if ($expected.ToLower() -ne $actual) {
        throw "checksum mismatch for $asset (expected $expected, got $actual)"
      }
      Write-Host 'Checksum verified.'
    }
  } catch {
    if ($_.Exception.Message -like 'checksum mismatch*') { throw }
    # checksums.txt unavailable — skip verification
  }

  Expand-Archive -Path $zip -DestinationPath $tmp -Force
  $bin = Join-Path $tmp 'dcms.exe'
  if (-not (Test-Path $bin)) { throw 'archive did not contain dcms.exe' }

  # ── Install ───────────────────────────────────────────────────────────────
  $dir = $env:DCMS_INSTALL_DIR
  if ([string]::IsNullOrEmpty($dir)) { $dir = Join-Path $env:LOCALAPPDATA 'dcms\bin' }
  New-Item -ItemType Directory -Path $dir -Force | Out-Null
  Copy-Item -Path $bin -Destination (Join-Path $dir 'dcms.exe') -Force

  $ver = & (Join-Path $dir 'dcms.exe') version 2>$null
  Write-Host "Installed $ver to $dir\dcms.exe"

  # Add to the user PATH if it isn't already there.
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (($userPath -split ';') -notcontains $dir) {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$dir", 'User')
    Write-Host "Added $dir to your user PATH — open a new terminal for it to take effect."
  }

  Write-Host ''
  Write-Host 'Next:  dcms init myapp; cd myapp; dcms dev'
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
