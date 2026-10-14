param(
    [Parameter(Mandatory=$true)][string]$InstallDir,
    [Parameter(Mandatory=$true)][string]$NssmPath
)
$ErrorActionPreference = 'Stop'
$InstallDir = (Resolve-Path $InstallDir).Path
$NssmPath = (Resolve-Path $NssmPath).Path
if ($InstallDir.Contains('"')) { throw 'InstallDir cannot contain quotes' }
function Invoke-Checked([string]$Program, [string[]]$Arguments) {
    & $Program @Arguments | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Dependency operation failed: $Program ($LASTEXITCODE)" }
}
$manifest = Get-Content -Raw (Join-Path $InstallDir 'versions.json') | ConvertFrom-Json
$files = @{
    'metabase.jar' = Join-Path $InstallDir 'metabase\metabase.jar'
    'caddy.exe' = Join-Path $InstallDir 'bin\caddy.exe'
    'java.exe' = Join-Path $InstallDir 'java\bin\java.exe'
    'nssm.exe' = $NssmPath
    'bvk-analytics.exe' = Join-Path $InstallDir 'bin\bvk-analytics.exe'
}
foreach ($name in $files.Keys) {
    $expected = $manifest.sha256.PSObject.Properties[$name].Value
    if ($expected -notmatch '^[a-fA-F0-9]{64}$') { throw "Verified SHA256 required for $name" }
    if ((Get-FileHash -Algorithm SHA256 $files[$name]).Hash -ne $expected) { throw "SHA256 mismatch for $name" }
}
foreach ($name in @('config.json','Caddyfile','run-service.ps1')) {
    if (!(Test-Path (Join-Path $InstallDir $name) -PathType Leaf)) { throw "Missing $name" }
}
$services = @{ analytics = 'BVKAnalytics'; metabase = 'BVKMetabase'; caddy = 'BVKCaddy' }
foreach ($name in $services.Values) {
    if (Get-Service -Name $name -ErrorAction SilentlyContinue) { throw "Service already exists: $name. Stop and update it separately." }
}
# Protect root before creating any secret. Local Administrators and SYSTEM only.
Invoke-Checked 'icacls.exe' @($InstallDir, '/inheritance:r', '/grant:r', '*S-1-5-32-544:(OI)(CI)F', '*S-1-5-18:(OI)(CI)F')
New-Item -ItemType Directory -Force (Join-Path $InstallDir 'secrets') | Out-Null
Invoke-Checked 'icacls.exe' @((Join-Path $InstallDir 'secrets'), '/inheritance:r', '/grant:r', '*S-1-5-32-544:(OI)(CI)F', '*S-1-5-18:(OI)(CI)F')
$powershell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
foreach ($component in @('analytics','metabase','caddy')) {
    $name = $services[$component]
    $identity = "NT SERVICE\$name"
    $parameters = '-NoProfile -NonInteractive -ExecutionPolicy RemoteSigned -File "' + (Join-Path $InstallDir 'run-service.ps1') + '" -Component ' + $component + ' -InstallDir "' + $InstallDir + '"'
    Invoke-Checked $NssmPath @('install', $name, $powershell, $parameters)
    Invoke-Checked $NssmPath @('set', $name, 'ObjectName', $identity)
    Invoke-Checked $NssmPath @('set', $name, 'AppDirectory', $InstallDir)
    Invoke-Checked $NssmPath @('set', $name, 'AppExit', 'Default', 'Restart')
    Invoke-Checked $NssmPath @('set', $name, 'AppRestartDelay', '10000')
    Invoke-Checked $NssmPath @('set', $name, 'AppStopMethodConsole', '30000')
    Invoke-Checked $NssmPath @('set', $name, 'AppKillProcessTree', '1')
    Invoke-Checked $NssmPath @('set', $name, 'Start', 'SERVICE_DEMAND_START')
    Invoke-Checked 'sc.exe' @('sidtype', $name, 'unrestricted')
    # RX for immutable binaries/configuration; secrets directory has no inheritance.
    Invoke-Checked 'icacls.exe' @($InstallDir, '/grant', ($identity + ':(OI)(CI)RX'))
    Invoke-Checked 'icacls.exe' @((Join-Path $InstallDir 'secrets'), '/grant', ($identity + ':(RX)'))
    $secret = Join-Path $InstallDir "secrets\$component.json"
    if (!(Test-Path $secret)) { '{}' | Set-Content -Encoding UTF8 $secret }
    Invoke-Checked 'icacls.exe' @($secret, '/inheritance:r', '/grant:r', '*S-1-5-32-544:F', '*S-1-5-18:F', ($identity + ':R'))
    $logs = Join-Path $InstallDir "data\logs\$component"
    New-Item -ItemType Directory -Force $logs | Out-Null
    Invoke-Checked 'icacls.exe' @($logs, '/grant', ($identity + ':(OI)(CI)M'))
    Invoke-Checked $NssmPath @('set', $name, 'AppStdout', (Join-Path $logs 'stdout.log'))
    Invoke-Checked $NssmPath @('set', $name, 'AppStderr', (Join-Path $logs 'stderr.log'))
    Invoke-Checked $NssmPath @('set', $name, 'AppRotateFiles', '1')
    Invoke-Checked $NssmPath @('set', $name, 'AppRotateOnline', '1')
    Invoke-Checked $NssmPath @('set', $name, 'AppRotateBytes', '10485760')
    if ($component -eq 'caddy') {
        foreach ($dir in @('data\caddy','data\caddy-config')) {
            $path = Join-Path $InstallDir $dir
            New-Item -ItemType Directory -Force $path | Out-Null
            Invoke-Checked 'icacls.exe' @($path, '/grant', ($identity + ':(OI)(CI)M'))
        }
    }
}
Write-Host 'Installed stopped services. Fill protected secrets, complete runbook checks, then enable automatic start.'
