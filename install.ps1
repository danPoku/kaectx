# Copyright 2026 Dan Gyinaye Poku
# SPDX-License-Identifier: Apache-2.0

<#
Installs a prebuilt kae on Windows:

  irm https://raw.githubusercontent.com/danPoku/kaectx/master/install.ps1 | iex

It downloads the Windows archive from the GitHub release, checks it against
the release's SHA256SUMS, and copies kae.exe into a per-user folder that it
adds to the user's PATH. Nothing is installed if the checksum does not match.

Settings (environment variables):
  KAE_VERSION         release tag to install, e.g. v0.3.0 (default: the latest)
  KAE_INSTALL_DIR     where to put kae.exe (default: %LOCALAPPDATA%\Programs\kae)
  KAE_BASE_URL        a URL or local folder to take the files from instead of
                      GitHub; used by CI to test this script
  KAE_NO_PATH_UPDATE  set to 1 to leave the user's PATH alone

To build from source instead, see scripts\install-windows.ps1.
#>

# Everything runs inside one function called on the last line, so a download
# cut off half way runs nothing, and so the script's variables do not leak
# into the session when it is piped to iex.
function Install-Kae {
    $ErrorActionPreference = 'Stop'
    $repo = 'danPoku/kaectx'

    if ($env:PROCESSOR_ARCHITECTURE -ne 'AMD64' -and $env:PROCESSOR_ARCHITEW6432 -ne 'AMD64') {
        throw "No prebuilt kae for $($env:PROCESSOR_ARCHITECTURE) Windows. Build from source (see the README)."
    }
    $asset = 'kae_windows_amd64.zip'

    if ($env:KAE_BASE_URL) { $base = $env:KAE_BASE_URL.TrimEnd('/', '\') }
    elseif ($env:KAE_VERSION) { $base = "https://github.com/$repo/releases/download/$($env:KAE_VERSION)" }
    else { $base = "https://github.com/$repo/releases/latest/download" }

    if ($env:KAE_INSTALL_DIR) { $dest = $env:KAE_INSTALL_DIR }
    else { $dest = Join-Path $env:LOCALAPPDATA 'Programs\kae' }

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("kae-install-" + [guid]::NewGuid())
    New-Item -ItemType Directory -Force -Path $tmp | Out-Null
    try {
        # Get-ReleaseFile NAME: fetch one release file into $tmp, from a URL
        # or, when the base is a folder, by copying it.
        function Get-ReleaseFile([string]$Name) {
            $target = Join-Path $tmp $Name
            if ($base -match '^https?://') {
                $previous = $ProgressPreference
                $ProgressPreference = 'SilentlyContinue'   # the progress bar makes Windows PowerShell downloads very slow
                try { Invoke-WebRequest -Uri "$base/$Name" -OutFile $target -UseBasicParsing }
                finally { $ProgressPreference = $previous }
            }
            else { Copy-Item -LiteralPath (Join-Path $base $Name) -Destination $target }
            return $target
        }

        Write-Host "Downloading $asset"
        $archive = Get-ReleaseFile $asset
        $sums = Get-ReleaseFile 'SHA256SUMS'

        # The checksum file lists every archive of the release; take this
        # one's line. No line is treated like a wrong checksum: there is
        # nothing to trust the download against.
        $want = $null
        foreach ($line in Get-Content -LiteralPath $sums) {
            $parts = $line -split '\s+', 2
            if ($parts.Count -eq 2 -and $parts[1].TrimStart('*').Trim() -eq $asset) { $want = $parts[0].ToLowerInvariant() }
        }
        if (-not $want) { throw "SHA256SUMS has no entry for $asset; not installing." }
        $got = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($want -ne $got) { throw "Checksum mismatch for ${asset}: expected $want, got $got. Not installing." }

        $pkg = Join-Path $tmp 'pkg'
        Expand-Archive -LiteralPath $archive -DestinationPath $pkg -Force
        if (-not (Test-Path (Join-Path $pkg 'kae.exe'))) { throw 'The archive does not contain kae.exe.' }

        New-Item -ItemType Directory -Force -Path $dest | Out-Null
        # `ctx` is the command's old name, kept as a deprecated alias. It is
        # refreshed only where one is already installed.
        $names = @('kae')
        if ((Test-Path (Join-Path $dest 'ctx.exe')) -and (Test-Path (Join-Path $pkg 'ctx.exe'))) { $names += 'ctx' }
        foreach ($name in $names) {
            try { Copy-Item -LiteralPath (Join-Path $pkg "$name.exe") -Destination (Join-Path $dest "$name.exe") -Force }
            catch {
                # Windows will not replace a running executable.
                throw "Could not replace $dest\$name.exe ($($_.Exception.Message)). Close the agents and stop any daemon using it, then run this again."
            }
        }

        if ($env:KAE_NO_PATH_UPDATE -ne '1') {
            $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
            $entries = @($userPath -split ';' | Where-Object { $_ })
            if ($entries -notcontains $dest) {
                [Environment]::SetEnvironmentVariable('Path', (($entries + $dest) -join ';'), 'User')
                $env:Path = "$env:Path;$dest"
                Write-Host "Added $dest to your PATH. Open a new terminal for other programs to see it."
            }
        }

        $version = & (Join-Path $dest 'kae.exe') version
        Write-Host "Installed $version to $dest\kae.exe"
        Write-Host 'Next: kae ingest, then kae search "something you worked on"'
    }
    finally { Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue }
}

Install-Kae
