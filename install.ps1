# sonarless-mcp installer for Windows.
#
#   irm https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install.ps1 | iex
#   & ([scriptblock]::Create((irm .../install.ps1))) --yes   # args go to `sonarless-mcp setup`
#
# Detects the CPU, downloads the matching release, verifies its checksum (and,
# when cosign is installed, the checksums file's Sigstore signature by this
# repository's release workflow), installs to %LOCALAPPDATA%\Programs\sonarless-mcp
# (added to your user PATH), then opens the client picker (`sonarless-mcp setup`).
# Later upgrades: `sonarless-mcp upgrade`.
#
# Env: SONARLESS_MCP_VERSION (tag, default latest), SONARLESS_MCP_INSTALL_DIR,
#      SONARLESS_MCP_BASE_URL (download base; for mirrors and tests),
#      SONARLESS_MCP_NO_SETUP=1 (install only).
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'BrainerVirus/sonarless-mcp'
# Who signs checksums.txt: release.yml on main (semantic-release runs on the
# push to main). internal/update.ReleaseIdentity pins the same identity.
$signerIssuer = 'https://token.actions.githubusercontent.com'
$signerIdentity = '^https://github\.com/BrainerVirus/sonarless-mcp/\.github/workflows/release\.yml@refs/heads/main$'
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

    if (Get-Command cosign -ErrorAction SilentlyContinue) {
        $bundle = $null
        try { $bundle = Get-File 'checksums.txt.sigstore.json' } catch { $bundle = $null }
        # Releases up to v0.8.1 were published before signing existed.
        $preSigning = $false
        if ($env:SONARLESS_MCP_VERSION) {
            try { $preSigning = [version](($env:SONARLESS_MCP_VERSION.TrimStart('v')) -replace '-.*$', '') -le [version]'0.8.1' } catch { $preSigning = $false }
        }
        if ($bundle) {
            & cosign verify-blob --bundle $bundle --certificate-oidc-issuer $signerIssuer `
                --certificate-identity-regexp $signerIdentity $sums *> $null
            if ($LASTEXITCODE -ne 0) { throw 'the signature of checksums.txt does not verify; nothing was installed' }
            Write-Host 'Verified the cosign signature of checksums.txt.'
        } elseif ($preSigning) {
            Write-Host "$($env:SONARLESS_MCP_VERSION) was published before releases were signed; checking its SHA-256 only."
        } else {
            throw 'the release has no signature (checksums.txt.sigstore.json); nothing was installed'
        }
    } else {
        Write-Host 'cosign not found; skipping the signature check (the SHA-256 is still checked).'
    }

    $line = Get-Content $sums | Where-Object { $_ -match " $([regex]::Escape($asset))$" } | Select-Object -First 1
    if (-not $line) { throw "$asset not listed in checksums.txt" }
    $want = ($line -split '\s+')[0]
    $got = (Get-FileHash -Algorithm SHA256 $zip).Hash.ToLower()
    if ($got -ne $want.ToLower()) { throw "checksum mismatch for $asset" }
    Write-Host 'Checksum OK.'

    Expand-Archive -Path $zip -DestinationPath (Join-Path $tmp 'x') -Force
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    $exe = Join-Path $installDir 'sonarless-mcp.exe'
    # A running exe can't be overwritten on Windows but can be renamed aside;
    # if the old copy is itself still running, pick a fresh name.
    $old = $null
    if (Test-Path $exe) {
        $old = "$exe.old"
        Remove-Item $old -Force -ErrorAction SilentlyContinue
        if (Test-Path $old) { $old = "$exe.old-$([DateTime]::UtcNow.Ticks)" }
        Rename-Item $exe (Split-Path $old -Leaf)
    }
    try {
        Copy-Item (Join-Path $tmp 'x\sonarless-mcp.exe') $exe -ErrorAction Stop
    } catch {
        if ($old) { Rename-Item $old (Split-Path $exe -Leaf) }  # put the previous version back
        throw
    }
    Write-Host "Installed $(& $exe --version) to $exe"

    # Edit the user PATH in the registry as stored (REG_EXPAND_SZ, %VARS%
    # unexpanded): [Environment]::GetEnvironmentVariable would expand them.
    $envKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    $userPath = [string]$envKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    if (($userPath -split ';') -notcontains $installDir) {
        $envKey.SetValue('Path', ($userPath.TrimEnd(';') + ";$installDir").TrimStart(';'), [Microsoft.Win32.RegistryValueKind]::ExpandString)
        # Let Explorer and new terminals know (a no-op variable change broadcasts WM_SETTINGCHANGE).
        [Environment]::SetEnvironmentVariable('SONARLESS_MCP_PATH_REFRESH', '1', 'User')
        [Environment]::SetEnvironmentVariable('SONARLESS_MCP_PATH_REFRESH', $null, 'User')
        $env:Path += ";$installDir"
        Write-Host "Added $installDir to your user PATH (new terminals pick it up)."
    }
    $envKey.Close()
    if ($env:TERM_PROGRAM -eq 'WarpTerminal') {
        Write-Host 'If Warp underlines sonarless-mcp as unknown, open a new tab (Warp reads PATH only when a tab starts; the command already works here).'
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
else {
    # No terminal (an AI agent or a script): show what was found and how to finish.
    & $exe setup --list
    Write-Host ''
    Write-Host 'Next: register it in the clients you want, e.g.'
    Write-Host '  sonarless-mcp setup --clients claude,cursor    (or --yes for every detected client)'
    Write-Host 'then restart those clients.'
}
