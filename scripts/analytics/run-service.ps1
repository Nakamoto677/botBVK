param(
    [Parameter(Mandatory=$true)][ValidateSet('analytics','metabase','caddy')][string]$Component,
    [Parameter(Mandatory=$true)][string]$InstallDir
)
$ErrorActionPreference = 'Stop'
try {
    Set-Location $InstallDir
    # Files have restricted ACLs, established BEFORE secrets are written.
    # Never print the file or put its values on a child command line.
    $values = Get-Content -Raw (Join-Path $InstallDir "secrets\$Component.json") | ConvertFrom-Json
    $allowed = @{
        analytics = @('DATABASE_URL')
        metabase = @('MB_DB_HOST','MB_DB_PORT','MB_DB_DBNAME','MB_DB_USER','MB_DB_PASS','MB_ENCRYPTION_SECRET_KEY','MB_SITE_URL')
        caddy = @('ANALYTICS_DOMAIN','ACME_EMAIL')
    }
    foreach ($name in $allowed[$Component]) {
        $value = $values.PSObject.Properties[$name].Value
        if ([string]::IsNullOrWhiteSpace($value)) { throw "Required environment value absent: $name" }
        [Environment]::SetEnvironmentVariable($name, $value, 'Process')
    }
    # Do not inherit the bot's token or privileged DB URL.
    Remove-Item Env:TELEGRAM_BOT_TOKEN -ErrorAction SilentlyContinue
    if ($Component -ne 'analytics') { Remove-Item Env:DATABASE_URL -ErrorAction SilentlyContinue }
    switch ($Component) {
        analytics { & '.\bin\bvk-analytics.exe' -config '.\config.json' }
        metabase {
            $env:MB_DB_TYPE = 'postgres'
            $env:MB_JETTY_HOST = '127.0.0.1'
            $env:MB_JETTY_PORT = '3000'
            $env:MB_ENABLE_PUBLIC_SHARING = 'false'
            $env:MB_ENABLE_EMBEDDING = 'false'
            $env:MB_LOAD_SAMPLE_CONTENT = 'false'
            $env:MB_REPORT_TIMEZONE = 'Asia/Irkutsk'
            & '.\java\bin\java.exe' '-Xmx2g' '--add-opens' 'java.base/java.nio=ALL-UNNAMED' '-jar' '.\metabase\metabase.jar'
        }
        caddy {
            $env:XDG_DATA_HOME = Join-Path $InstallDir 'data\caddy'
            $env:XDG_CONFIG_HOME = Join-Path $InstallDir 'data\caddy-config'
            & '.\bin\caddy.exe' run --config '.\Caddyfile' --adapter caddyfile
        }
    }
    exit $LASTEXITCODE
} catch {
    # Omit exception details: dependency errors may include a connection URL.
    Write-Error "Service $Component could not start; verify protected configuration and dependencies." -ErrorAction Continue
    exit 1
}
