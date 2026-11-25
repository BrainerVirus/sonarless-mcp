# Signature matrix for install.ps1: a stub cosign on PATH, file:// fixtures
# built from a snapshot release, no network. CI runs it on Windows after the
# snapshot:
#
#   ./.github/scripts/test-install-signature.ps1 -Dist dist
#
# The same rows as test-install-signature.sh (install.sh). Each row runs
# install.ps1 in a child pwsh, so a refusal (a throw) is its exit code.
param([Parameter(Mandatory = $true)][string]$Dist)

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$installer = Join-Path $root 'install.ps1'
$dist = (Resolve-Path $Dist).Path
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "unsupported CPU: $($env:PROCESSOR_ARCHITECTURE)" }
}
$asset = "sonarless-mcp_windows_$arch.zip"

$w = Join-Path ([IO.Path]::GetTempPath()) ('sonarless-sigtest-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $w | Out-Null
function FileUri([string]$path) { ([uri]$path).AbsoluteUri }

# Releases: v0.8.1 (before signing, no bundle), v0.9.0 (unsigned, must be
# refused), v0.9.1 (with a bundle); releases/latest/download serves v0.9.0.
foreach ($tag in 'v0.8.1', 'v0.9.0', 'v0.9.1') {
    $dir = Join-Path $w "releases\download\$tag"
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    Copy-Item (Join-Path $dist $asset), (Join-Path $dist 'checksums.txt') $dir
    $api = Join-Path $w "api-$tag\releases"
    New-Item -ItemType Directory -Force -Path $api | Out-Null
    Set-Content -NoNewline -Path (Join-Path $api 'latest') -Value "{`"url`":`"x`",`"tag_name`":`"$tag`",`"name`":`"$tag`"}"
}
Set-Content -Path (Join-Path $w 'releases\download\v0.9.1\checksums.txt.sigstore.json') -Value '{}'
New-Item -ItemType Directory -Force -Path (Join-Path $w 'releases\latest') | Out-Null
Copy-Item -Recurse (Join-Path $w 'releases\download\v0.9.0') (Join-Path $w 'releases\latest\download')
foreach ($name in 'api-down', 'cosign-ok', 'cosign-bad') {
    New-Item -ItemType Directory -Force -Path (Join-Path $w $name) | Out-Null
}
Set-Content -Path (Join-Path $w 'cosign-ok\cosign.cmd') -Value '@exit 0'
Set-Content -Path (Join-Path $w 'cosign-bad\cosign.cmd') -Value '@exit 1'

$bin = Join-Path $w 'bin'
$basePath = $env:Path
$script:failed = $false

function Row([string]$name, [bool]$want, [string]$cosign, [string]$api, [string]$pinned, [string]$baseUrl = '') {
    if ($cosign -eq 'none') {
        if (Get-Command cosign -ErrorAction SilentlyContinue) {
            Write-Host "skip $name (cosign is installed on this machine)"
            return
        }
        $env:Path = $basePath
    } else {
        $env:Path = (Join-Path $w "cosign-$cosign") + ';' + $basePath
    }
    $env:SONARLESS_MCP_RELEASES_URL = FileUri (Join-Path $w 'releases')
    $env:SONARLESS_MCP_RELEASES_API_URL = FileUri (Join-Path $w $api)
    $env:SONARLESS_MCP_VERSION = $pinned
    $env:SONARLESS_MCP_BASE_URL = $baseUrl
    $env:SONARLESS_MCP_INSTALL_DIR = $bin
    $env:SONARLESS_MCP_NO_SETUP = '1'
    # The installer's own errors are expected output here, not this script's.
    $ErrorActionPreference = 'Continue'
    $out = & pwsh -NoProfile -NonInteractive -File $installer 2>&1 | Out-String
    $rc = $LASTEXITCODE
    $ErrorActionPreference = 'Stop'
    $env:Path = $basePath
    $got = ($rc -eq 0) -and (Test-Path (Join-Path $bin 'sonarless-mcp.exe'))
    if ($got -eq $want) {
        Write-Host "ok   $name"
    } else {
        Write-Host "FAIL ${name}: installed=$got (exit $rc), want $want"
        Write-Host ($out -replace '(?m)^', '     ')
        $script:failed = $true
    }
    Remove-Item -Recurse -Force $bin -ErrorAction SilentlyContinue
}

try {
    Row 'no cosign, latest is v0.8.1' $true 'none' 'api-v0.8.1' ''
    Row 'no cosign, API down (releases/latest)' $true 'none' 'api-down' ''
    Row 'no cosign, base URL (CI snapshot)' $true 'none' 'api-down' '' (FileUri (Join-Path $w 'releases\download\v0.9.0'))
    Row 'unsigned, latest is v0.8.1 (before signing)' $true 'ok' 'api-v0.8.1' ''
    Row 'unsigned, latest is v0.9.0' $false 'ok' 'api-v0.9.0' ''
    Row 'unsigned, API down (tag unknown)' $false 'ok' 'api-down' ''
    Row 'unsigned, pinned v0.8.1' $true 'ok' 'api-v0.9.0' 'v0.8.1'
    Row 'unsigned, pinned v0.9.0' $false 'ok' 'api-v0.8.1' 'v0.9.0'
    Row 'signed, latest v0.9.1, signature fails' $false 'bad' 'api-v0.9.1' ''
    Row 'signed, latest v0.9.1, signature verifies' $true 'ok' 'api-v0.9.1' ''
    Row 'signed, pinned v0.9.1, signature verifies' $true 'ok' 'api-v0.8.1' 'v0.9.1'
    Row 'signed, pinned v0.9.1, signature fails' $false 'bad' 'api-v0.8.1' 'v0.9.1'
} finally {
    foreach ($v in 'SONARLESS_MCP_RELEASES_URL', 'SONARLESS_MCP_RELEASES_API_URL', 'SONARLESS_MCP_VERSION',
        'SONARLESS_MCP_BASE_URL', 'SONARLESS_MCP_INSTALL_DIR', 'SONARLESS_MCP_NO_SETUP') {
        Remove-Item "Env:$v" -ErrorAction SilentlyContinue
    }
    Remove-Item -Recurse -Force $w -ErrorAction SilentlyContinue
}
if ($script:failed) { exit 1 }
# The Actions pwsh wrapper exits with $LASTEXITCODE, which the last refused
# install (exit 1) left set: end with an explicit success.
exit 0
