param(
    [Parameter(Mandatory=$true)][string]$BackupDir,
    [Parameter(Mandatory=$true)][string]$PgBin,
    [Parameter(Mandatory=$true)][ValidatePattern('^bvk_metabase_restore_[a-z0-9_]+$')][string]$TargetDatabase
)
$ErrorActionPreference = 'Stop'
if (!(Test-Path $env:PGPASSFILE -PathType Leaf)) { throw 'Protected PGPASSFILE required' }
$BackupDir = (Resolve-Path $BackupDir).Path
$manifest = Get-Content -Raw (Join-Path $BackupDir 'manifest.json') | ConvertFrom-Json
foreach ($file in $manifest) {
    $path = [IO.Path]::GetFullPath((Join-Path $BackupDir $file.path))
    if (!$path.StartsWith($BackupDir + '\',[StringComparison]::OrdinalIgnoreCase)) { throw 'Manifest path is outside backup' }
    if ((Get-FileHash $path -Algorithm SHA256).Hash -ne $file.sha256) { throw 'Backup integrity check failed' }
}
# Fails if the database already exists; never clean/drop a live database.
& (Join-Path $PgBin 'createdb.exe') -w --owner=bvk_metabase_app $TargetDatabase
if ($LASTEXITCODE -ne 0) { throw 'Cannot create isolated restore database; no existing database changed' }
& (Join-Path $PgBin 'psql.exe') -X -w --set=ON_ERROR_STOP=1 --dbname=$TargetDatabase --command="REVOKE ALL ON DATABASE $TargetDatabase FROM PUBLIC; REVOKE ALL ON SCHEMA public FROM PUBLIC; GRANT USAGE, CREATE ON SCHEMA public TO bvk_metabase_app;"
if ($LASTEXITCODE -ne 0) { throw 'Cannot restrict isolated restore database' }
& (Join-Path $PgBin 'pg_restore.exe') -w --exit-on-error --single-transaction --no-owner --no-privileges --role=bvk_metabase_app --dbname=$TargetDatabase (Join-Path $BackupDir 'metabase.dump')
if ($LASTEXITCODE -ne 0) { throw 'Restore failed; retain isolated database for investigation' }
& (Join-Path $PgBin 'psql.exe') -X -w --set=ON_ERROR_STOP=1 --dbname=$TargetDatabase --command='SELECT count(*) AS users FROM public.core_user; SELECT count(*) AS questions FROM public.report_card; SELECT count(*) AS dashboards FROM public.report_dashboard;'
if ($LASTEXITCODE -ne 0) { throw 'Restored Metabase application tables failed validation' }
Write-Host "Restored into $TargetDatabase. Perform isolated Metabase login/dashboard/export checks before manual cutover."
