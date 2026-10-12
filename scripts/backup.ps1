param([string]$InstallDir = (Split-Path $PSScriptRoot -Parent))
$ErrorActionPreference = "Stop"
Set-Location $InstallDir
# DATABASE_URL must be supplied by the scheduled task's protected environment.
& "./bin/bvk.exe" -config "config.json" -backup
if ($LASTEXITCODE -ne 0) { throw "Backup or restore verification failed; topic deletion stays blocked" }
