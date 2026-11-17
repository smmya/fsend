param(
    [string]$Version = '',
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'Programs\fsend'),
    [switch]$NoPath
)
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$repo = 'smmya/fsend'
$cpu = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
switch ($cpu) {
    'x64' { $arch = 'amd64' }
    'arm64' { $arch = 'arm64' }
    default { throw "Unsupported CPU architecture: $cpu" }
}
$api = "https://api.github.com/repos/$repo/releases"
if ($Version) {
    if ($Version -cnotmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') { throw 'Version must be vX.Y.Z' }
    $api += "/tags/$Version"
} else { $api += '/latest' }
$release = Invoke-RestMethod -Uri $api -Headers @{ 'User-Agent' = 'fsend-installer'; Accept = 'application/vnd.github+json' }
$Version = $release.tag_name
if ($Version -cnotmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') { throw 'Invalid release tag' }
$asset = "fsend_$($Version.Substring(1))_windows_$arch.zip"
$base = "https://github.com/$repo/releases/download/$Version"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ('fsend-' + [guid]::NewGuid().ToString('N'))
$stage = $null
try {
    New-Item -ItemType Directory -Path $tmp | Out-Null
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile (Join-Path $tmp $asset)
    Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')
    $lines = @(Get-Content -LiteralPath (Join-Path $tmp 'SHA256SUMS') | Where-Object { $_ -cmatch ('^[a-f0-9]{64}  ' + [regex]::Escape($asset) + '$') })
    if ($lines.Count -ne 1) { throw 'Checksum entry missing or invalid' }
    $expected = $lines[0].Substring(0, 64)
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tmp $asset)).Hash.ToLowerInvariant()
    if ($actual -cne $expected) { throw 'SHA-256 mismatch; installation cancelled' }
    Expand-Archive -LiteralPath (Join-Path $tmp $asset) -DestinationPath (Join-Path $tmp 'unpacked')
    $binary = Join-Path $tmp 'unpacked\fsend.exe'
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw 'Executable missing from package' }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $stage = Join-Path $InstallDir ('.fsend-' + [guid]::NewGuid().ToString('N') + '.exe')
    Copy-Item -LiteralPath $binary -Destination $stage
    Move-Item -Force -LiteralPath $stage -Destination (Join-Path $InstallDir 'fsend.exe')
    if (-not $NoPath) {
        $userPath = [string][Environment]::GetEnvironmentVariable('Path', 'User')
        if ($InstallDir -notin ($userPath -split ';')) {
            [Environment]::SetEnvironmentVariable('Path', (($userPath.TrimEnd(';') + ';' + $InstallDir).TrimStart(';')), 'User')
        }
        if ($InstallDir -notin ($env:Path -split ';')) { $env:Path += ";$InstallDir" }
    }
    Write-Output "Installed $Version to $InstallDir\fsend.exe. Open a new terminal to use fsend."
} finally {
    if ($stage -and (Test-Path -LiteralPath $stage)) { Remove-Item -Force -LiteralPath $stage }
    if (Test-Path -LiteralPath $tmp) { Remove-Item -Recurse -Force -LiteralPath $tmp }
}
