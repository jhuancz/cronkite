# scripts/install.ps1 — install cronkite on Windows.
#
#   .\install.ps1                          install to %ProgramFiles%\cronkite
#   .\install.ps1 -Prefix C:\tools         install somewhere else
#   .\install.ps1 -Register                also create the scheduled task
#   .\install.ps1 -WhatIfDryRun            show what would happen
#
# Installing and scheduling are separate on purpose: a job that deletes files
# should be enabled by the operator who configured it, not as a side effect of
# copying a binary.

[CmdletBinding()]
param(
    [string]$Prefix = "$env:ProgramFiles\cronkite",
    [string]$ConfigDir = "$env:ProgramData\cronkite",
    [string]$Binary = '',
    [switch]$Register,
    [string]$TaskName = 'cronkite',
    [switch]$WhatIfDryRun
)

$ErrorActionPreference = 'Stop'

function Invoke-Step {
    param([string]$Description, [scriptblock]$Action)
    if ($WhatIfDryRun) {
        Write-Host "would: $Description"
        return
    }
    Write-Host $Description
    & $Action
}

$repoRoot = Split-Path -Parent $PSScriptRoot

if (-not $Binary) {
    foreach ($candidate in @("$repoRoot\cronkite.exe", (Get-Command cronkite.exe -ErrorAction SilentlyContinue).Source)) {
        if ($candidate -and (Test-Path -LiteralPath $candidate)) {
            $Binary = $candidate
            break
        }
    }
}
if (-not $Binary -or -not (Test-Path -LiteralPath $Binary)) {
    Write-Error 'install.ps1: no cronkite.exe found; build it or pass -Binary PATH'
    exit 1
}

Write-Host "binary:  $Binary"
Write-Host "install: $Prefix\cronkite.exe"
Write-Host "config:  $ConfigDir\cronkite.conf"

Invoke-Step "create $Prefix" { New-Item -ItemType Directory -Force -Path $Prefix | Out-Null }
Invoke-Step "copy cronkite.exe" { Copy-Item -LiteralPath $Binary -Destination "$Prefix\cronkite.exe" -Force }
Invoke-Step "copy cronkite-run.ps1" { Copy-Item -LiteralPath "$PSScriptRoot\cronkite-run.ps1" -Destination "$Prefix\cronkite-run.ps1" -Force }

Invoke-Step "create $ConfigDir" { New-Item -ItemType Directory -Force -Path $ConfigDir | Out-Null }
if (Test-Path -LiteralPath "$ConfigDir\cronkite.conf") {
    Write-Host "keeping existing $ConfigDir\cronkite.conf"
}
else {
    Invoke-Step "write an example configuration" {
        Copy-Item -LiteralPath "$repoRoot\cronkite.conf.example" -Destination "$ConfigDir\cronkite.conf"
    }
    Write-Host 'edit that file before scheduling anything'
}

if ($Register) {
    # schtasks is the dependency-free way to do this; the wrapper exists so a
    # locked run (exit 3) is not recorded as a failure.
    $action = "powershell.exe -NoProfile -File `"$Prefix\cronkite-run.ps1`""
    Invoke-Step "register the scheduled task $TaskName (daily at 03:30)" {
        schtasks.exe /create /tn $TaskName /tr $action /sc daily /st 03:30 /f | Out-Null
    }
}

@"

Installed. Next:

  1. Edit $ConfigDir\cronkite.conf
  2. Check it:   "$Prefix\cronkite.exe" check -config "$ConfigDir\cronkite.conf"
  3. Rehearse:   "$Prefix\cronkite.exe" plan -config "$ConfigDir\cronkite.conf"
  4. Schedule:   .\install.ps1 -Register      (or schtasks; see docs\scheduling.md)

"@ | Write-Host
