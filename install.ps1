# sonarless-mcp installer for Windows.
#
#   irm https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install.ps1 | iex
#   & ([scriptblock]::Create((irm .../install.ps1))) --yes   # args go to `sonarless-mcp setup`
#
# Detects the CPU, downloads the matching release, verifies its checksum,
# installs to %LOCALAPPDATA%\Programs\sonarless-mcp (added to your user PATH),
# then opens the client picker (`sonarless-mcp setup`).
#
# Env: SONARLESS_MCP_VERSION (tag, default latest), SONARLESS_MCP_INSTALL_DIR,
#      SONARLESS_MCP_BASE_URL (download base; for mirrors and tests),
#      SONARLESS_MCP_NO_SETUP=1 (install only).
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'BrainerVirus/sonarless-mcp'
$installDir = if ($env:SONARLESS_MCP_INSTALL_DIR) { $env:SONARLESS_MCP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\sonarless-mcp' }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "unsupported CPU: $($env:PROCESSOR_ARCHITECTURE)" }
}

$base = if ($env:SONARLESS_MCP_BASE_URL) { $env:SONARLESS_MCP_BASE_URL }
        elseif ($env:SONARLESS_MCP_VERSION) { "https://github.com/$repo/releases/download/$($env:SONARLESS_MCP_VERSION)" }
        else { "https://github.com/$repo/releases/latest/download" }

$asset = "sonarless-mcp_windows_$arch.zip"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("sonarless-mcp-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Detected windows/$arch. Downloading $asset..."
    function Get-File($name) {
        $dest = Join-Path $tmp $name
        if ($base -like 'file://*') { Copy-Item ([uri]"$base/$name").LocalPath $dest }
        else { Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile $dest }
        $dest
    }
    $zip = Get-File $asset
    $sums = Get-File 'checksums.txt'

    $line = Get-Content $sums | Where-Object { $_ -match " $([regex]::Escape($asset))$" } | Select-Object -First 1
    if (-not $line) { throw "$asset not listed in checksums.txt" }
    $want = ($line -split '\s+')[0]
    $got = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLower()
    if ($got -ne $want.ToLower()) { throw "checksum mismatch for $asset" }
    Write-Host 'Checksum OK.'

    Expand-Archive -Path $zip -DestinationPath (Join-Path $tmp 'x') -Force
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    $exe = Join-Path $installDir 'sonarless-mcp.exe'
    # A running exe can't be overwritten on Windows but can be renamed aside.
    if (Test-Path $exe) {
        $old = "$exe.old"
        Remove-Item $old -Force -ErrorAction SilentlyContinue
        Rename-Item $exe (Split-Path $old -Leaf)
    }
    Copy-Item (Join-Path $tmp 'x\sonarless-mcp.exe') $exe
    Write-Host "Installed $(& $exe --version) to $exe"

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $installDir) {
        [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ";$installDir").TrimStart(';'), 'User')
        $env:Path += ";$installDir"
        Write-Host "Added $installDir to your user PATH (new terminals pick it up)."
    }
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        Write-Host 'note: Docker not found; sonarless-mcp needs Docker Desktop to run SonarQube.'
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

if ($env:SONARLESS_MCP_NO_SETUP -eq '1') { return }
Write-Host ''
if ($args.Count -gt 0) { & $exe setup @args }
elseif ([Environment]::UserInteractive -and -not [Console]::IsInputRedirected) { & $exe setup }
else { Write-Host 'Run `sonarless-mcp setup` to register it in your AI clients.' }
