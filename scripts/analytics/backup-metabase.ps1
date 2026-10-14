param(
    [Parameter(Mandatory=$true)][string]$InstallDir,
    [Parameter(Mandatory=$true)][string]$BackupRoot,
    [Parameter(Mandatory=$true)][string]$PgBin
)
$ErrorActionPreference = 'Stop'
# Use PGHOST/PGPORT/PGUSER and protected PGPASSFILE, never a password argument.
if (!(Test-Path $env:PGPASSFILE -PathType Leaf)) { throw 'Protected PGPASSFILE required' }
$backup = Join-Path $BackupRoot ([DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ') + '-' + [Guid]::NewGuid().ToString('N').Substring(0,8))
New-Item -ItemType Directory $backup | Out-Null
& icacls.exe $backup /inheritance:r /grant:r '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Cannot protect backup directory' }
$running = (Get-Service BVKMetabase).Status -eq 'Running'
try {
    if ($running) { Stop-Service BVKMetabase; (Get-Service BVKMetabase).WaitForStatus('Stopped',[TimeSpan]::FromSeconds(60)) }
    & (Join-Path $PgBin 'pg_dump.exe') -w --format=custom --dbname=bvk_metabase --file=(Join-Path $backup 'metabase.dump')
    if ($LASTEXITCODE -ne 0) { throw 'pg_dump failed; backup is incomplete' }
    & (Join-Path $PgBin 'pg_restore.exe') --list (Join-Path $backup 'metabase.dump') | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Dump archive cannot be read' }
    foreach ($name in @('config.json','Caddyfile','versions.json','run-service.ps1')) {
        Copy-Item (Join-Path $InstallDir $name) $backup
    }
    if (Test-Path (Join-Path $InstallDir 'import-state.json')) {
        Copy-Item (Join-Path $InstallDir 'import-state.json') $backup
    }
    Copy-Item (Join-Path $InstallDir 'secrets') (Join-Path $backup 'secrets') -Recurse
    Get-ChildItem $backup -Recurse -File | ForEach-Object {
        [PSCustomObject]@{ path = $_.FullName.Substring($backup.Length + 1); sha256 = (Get-FileHash $_.FullName -Algorithm SHA256).Hash }
    } | ConvertTo-Json | Set-Content -Encoding UTF8 (Join-Path $backup 'manifest.json')
    Write-Host "Backup saved: $backup. Archive readability verified; full restore drill still required."
} finally {
    if ($running) { Start-Service BVKMetabase }
}
