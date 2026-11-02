<#
.SYNOPSIS
    A site maintenance script driven by cronkite (Windows).

.DESCRIPTION
    cronkite runs this file as a `script` task with interpreter `powershell`,
    so it inherits the run's lock, its report and its rehearsal mode.

    The contract it receives:
      CRONKITE_RUN_ID    the id of the report this run writes
      CRONKITE_MODE      plan or apply
      CRONKITE_DRY_RUN   1 during a rehearsal, 0 when the run may change things
      CRONKITE_TASK      the task name from the configuration

    Two rules make a script safe to schedule:
      - honour CRONKITE_DRY_RUN; a rehearsal that deletes something is a bug
      - exit non-zero when something went wrong; cronkite records the status
        and continues with the next task

.EXAMPLE
    $env:CRONKITE_DRY_RUN = '1'; $env:LOG_DIR = 'C:\Temp\demo'; .\maintenance.ps1
#>

$ErrorActionPreference = 'Stop'

$logDir = if ($env:LOG_DIR) { $env:LOG_DIR } else { 'C:\Logs\MyApp' }
$keepDays = if ($env:KEEP_DAYS) { [int]$env:KEEP_DAYS } else { 14 }
$dryRun = $env:CRONKITE_DRY_RUN -ne '0'

Write-Output ('{0}: run={1} mode={2} dry_run={3}' -f `
    $(if ($env:CRONKITE_TASK) { $env:CRONKITE_TASK } else { 'maintenance' }), `
    $(if ($env:CRONKITE_RUN_ID) { $env:CRONKITE_RUN_ID } else { 'none' }), `
    $(if ($env:CRONKITE_MODE) { $env:CRONKITE_MODE } else { 'manual' }), `
    $dryRun)

if (-not (Test-Path -LiteralPath $logDir)) {
    Write-Output "nothing to do: $logDir does not exist"
    exit 0
}

$cutoff = (Get-Date).AddDays(-$keepDays)
$stale = @(Get-ChildItem -LiteralPath $logDir -Filter *.log -File -Recurse |
    Where-Object { $_.LastWriteTime -lt $cutoff })

foreach ($file in $stale) {
    if ($dryRun) {
        Write-Output "would remove $($file.FullName)"
    }
    else {
        Remove-Item -LiteralPath $file.FullName -Force
        Write-Output "removed $($file.FullName)"
    }
}

Write-Output ('{0} file(s) older than {1}d under {2}' -f $stale.Count, $keepDays, $logDir)
