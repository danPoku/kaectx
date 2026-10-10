# Copyright 2026 Dan Gyinaye Poku
# SPDX-License-Identifier: Apache-2.0

[CmdletBinding()]
param([switch]$RunTests, [switch]$StopRunningCtx)

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
    $destination = Join-Path $goBin 'ctx.exe'
    $candidate = Join-Path ([IO.Path]::GetTempPath()) ("ctx-{0}.exe" -f [guid]::NewGuid())
    try {
        & go build -tags sqlite_fts5 -o $candidate ./cmd/ctx
        if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
        $destinationPath = [IO.Path]::GetFullPath($destination)
        $running = @(Get-CimInstance Win32_Process -Filter "Name = 'ctx.exe'" | Where-Object {
            if (-not $_.ExecutablePath) { return $false }
            try {
                $processPath = (Resolve-Path -LiteralPath $_.ExecutablePath).Path
                return [string]::Equals($processPath, $destinationPath, [StringComparison]::OrdinalIgnoreCase)
            }
            catch { return $false }
        })
        if ($running.Count -gt 0 -and -not $StopRunningCtx) {
            throw "ctx.exe is running ($($running.ProcessId -join ', ')). Close its MCP sessions or rerun with -StopRunningCtx."
        }
        if ($StopRunningCtx) {
            $running | ForEach-Object {
                Stop-Process -Id $_.ProcessId -Force -PassThru | Wait-Process -Timeout 10
            }
        }
        if (Test-Path $destination) {
            $stamp = Get-Date -Format 'yyyyMMdd-HHmmssfff'
            $backup = "$destination.backup-$stamp"
            Copy-Item -LiteralPath $destination -Destination $backup
        }
        Copy-Item -LiteralPath $candidate -Destination $destination -Force
        Write-Host "Installed $destination"
        if ($backup) { Write-Host "Previous build: $backup" }
    }
    finally { Remove-Item -LiteralPath $candidate -ErrorAction SilentlyContinue }
}
finally { Pop-Location }
