# agentlink installer for Windows. Run it with:
#   irm https://raw.githubusercontent.com/Boughzala-ayoub/agentlink/main/install.ps1 | iex
# It downloads one executable, checks its SHA-256 against the release's
# checksums.txt, and runs `agentlink setup`, which shows what it will change
# and asks once. Nothing else is installed and nothing keeps running.
& {
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'
  $repo = if ($env:AGENTLINK_REPO) { $env:AGENTLINK_REPO } else { 'Boughzala-ayoub/agentlink' }
  $base = if ($env:AGENTLINK_BASE_URL) { $env:AGENTLINK_BASE_URL } else { "https://github.com/$repo/releases/latest/download" }
  $arch = 'amd64'
  try { if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { $arch = 'arm64' } } catch {}
  $asset = "agentlink-windows-$arch.exe"
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('agentlink-' + [guid]::NewGuid())
  New-Item -ItemType Directory $tmp | Out-Null
  try {
    $exe = Join-Path $tmp 'agentlink.exe'
    Write-Host "Downloading $asset ..."
    Invoke-WebRequest "$base/$asset" -OutFile $exe -UseBasicParsing
    $sums = (Invoke-WebRequest "$base/checksums.txt" -UseBasicParsing).Content
    if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
    $line = $sums -split "`r?`n" | Where-Object { ($_ -split '\s+')[1] -in @($asset, "*$asset") } | Select-Object -First 1
    if (-not $line) { throw "checksums.txt has no line for $asset. Nothing was installed." }
    $want = ($line -split '\s+')[0].ToLower()
    $got = (Get-FileHash $exe -Algorithm SHA256).Hash.ToLower()
    if ($got -ne $want) { throw "Checksum mismatch for ${asset}: got $got, expected $want. Nothing was installed." }
    # setup copies itself to ~\.agentlink\bin and writes that path into each app.
    & $exe setup
    if ($LASTEXITCODE) { Write-Host "agentlink setup exited with code $LASTEXITCODE." -ForegroundColor Red }
  } catch {
    Write-Host "agentlink: $($_.Exception.Message)" -ForegroundColor Red
  } finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
}
