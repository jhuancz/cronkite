# scripts/cronkite-run.ps1 — run cronkite from Task Scheduler on Windows.
#
# Why a wrapper instead of pointing the task straight at cronkite.exe: exit code 3
# means "another run holds the lock", which Task Scheduler would record as a
# failure and surface as a broken task. Every other non-zero code is a real signal
# and is passed through unchanged.
#
#   schtasks /create /tn cronkite /tr "powershell -File C:\cronkite\cronkite-run.ps1" /sc daily /st 03:30
#
# Configuration is read from the standard locations, or from CRONKITE_CONFIG.

[CmdletBinding()]
param(
    # The wrapper ships next to the binary it runs, so an install under any prefix
    # works without the scheduler having to set anything.
    [string]$CronkiteBin = (Join-Path $PSScriptRoot 'cronkite.exe'),
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$Arguments = @()
)

$ErrorActionPreference = 'Stop'

if (-not (Test-Path -LiteralPath $CronkiteBin)) {
    Write-Error "cronkite-run: $CronkiteBin not found; install cronkite or pass -CronkiteBin"
    exit 1
}

$invokeArgs = @('run', '--apply')
if ($env:CRONKITE_CONFIG) {
    $invokeArgs += @('-config', $env:CRONKITE_CONFIG)
}
$invokeArgs += $Arguments

& $CronkiteBin @invokeArgs
$code = $LASTEXITCODE

switch ($code) {
    0 { exit 0 }
    3 {
        # A previous run still holds the lock. Expected on a busy host and not
        # worth a failed task in the scheduler.
        Write-Output 'cronkite-run: previous run still in progress; nothing to do'
        exit 0
    }
    default {
        # 1 usage, 2 partial, 4 refused: all worth a human.
        exit $code
    }
}
