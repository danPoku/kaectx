# Copyright 2026 Dan Gyinaye Poku
# SPDX-License-Identifier: Apache-2.0

# Builds kae.exe, and ctx.exe (the deprecated alias for the old command name),
# and installs both into the Go bin directory. -StopRunningCtx is the old name
# of -StopRunning and still works.
[CmdletBinding()]
param([switch]$RunTests, [Alias('StopRunningCtx')][switch]$StopRunning)

$ErrorActionPreference = 'Stop'

function Find-GccDirectory {
    $gcc = Get-Command gcc.exe -ErrorAction SilentlyContinue
    if ($gcc) { return Split-Path -Parent $gcc.Source }
    foreach ($candidate in @('C:\msys64\ucrt64\bin', 'C:\msys64\mingw64\bin')) {
        if (Test-Path (Join-Path $candidate 'gcc.exe')) { return $candidate }
    }
    throw @'
No MinGW gcc compiler was found. Install the MSYS2 UCRT64 toolchain, then retry:
  winget install MSYS2.MSYS2
  C:\msys64\usr\bin\bash.exe -lc "pacman -S --needed --noconfirm mingw-w64-ucrt-x86_64-gcc"
'@
}

# Returns the running processes whose executable is exactly $Path. A kae.exe
# somewhere else on the machine is not ours to stop.
function Get-RunningFrom([string]$Name, [string]$Path) {
    @(Get-CimInstance Win32_Process -Filter "Name = '$Name'" | Where-Object {
        if (-not $_.ExecutablePath) { return $false }
        try {
            $processPath = (Resolve-Path -LiteralPath $_.ExecutablePath).Path
            return [string]::Equals($processPath, $Path, [StringComparison]::OrdinalIgnoreCase)
        }
        catch { return $false }
    })
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$gccDir = Find-GccDirectory
$env:Path = "$gccDir;$env:Path"
$env:CC = 'gcc'
$env:CGO_ENABLED = '1'

Push-Location $repoRoot
try {
    if ($RunTests) {
        & go test -count=1 -tags sqlite_fts5 ./...
        if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }
        & go vet -tags sqlite_fts5 ./...
        if ($LASTEXITCODE -ne 0) { throw "go vet failed with exit code $LASTEXITCODE" }
    }
    $goBin = (& go env GOBIN).Trim()
    if (-not $goBin) {
        $goPath = ((& go env GOPATH).Trim() -split [IO.Path]::PathSeparator)[0]
        $goBin = Join-Path $goPath 'bin'
    }
    New-Item -ItemType Directory -Force -Path $goBin | Out-Null

    # Build both before replacing either, so a build failure leaves the
    # installed pair as it was.
    $names = @('kae', 'ctx')
    $candidates = @{}
    try {
        foreach ($name in $names) {
            $candidate = Join-Path ([IO.Path]::GetTempPath()) ("{0}-{1}.exe" -f $name, [guid]::NewGuid())
            $candidates[$name] = $candidate
            & go build -tags sqlite_fts5 -o $candidate "./cmd/$name"
            if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/$name failed with exit code $LASTEXITCODE" }
        }

        $running = @()
        foreach ($name in $names) {
            $destinationPath = [IO.Path]::GetFullPath((Join-Path $goBin "$name.exe"))
            $running += Get-RunningFrom "$name.exe" $destinationPath
        }
        if ($running.Count -gt 0 -and -not $StopRunning) {
            throw "kae is running ($($running.ProcessId -join ', ')). Close its MCP sessions or rerun with -StopRunning."
        }
        if ($StopRunning) {
            $running | ForEach-Object {
                Stop-Process -Id $_.ProcessId -Force -PassThru | Wait-Process -Timeout 10
            }
        }

        $stamp = Get-Date -Format 'yyyyMMdd-HHmmssfff'
        foreach ($name in $names) {
            $destination = Join-Path $goBin "$name.exe"
            $backup = $null
            if (Test-Path $destination) {
                $backup = "$destination.backup-$stamp"
                Copy-Item -LiteralPath $destination -Destination $backup
            }
            Copy-Item -LiteralPath $candidates[$name] -Destination $destination -Force
            Write-Host "Installed $destination"
            if ($backup) { Write-Host "Previous build: $backup" }
        }
        Write-Host "ctx.exe is a deprecated alias for kae.exe. Run 'kae migrate-agents' to move agent registrations over."
    }
    finally {
        foreach ($candidate in $candidates.Values) {
            Remove-Item -LiteralPath $candidate -ErrorAction SilentlyContinue
        }
    }
}
finally { Pop-Location }
