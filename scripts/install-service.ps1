param(
  [Parameter(Mandatory=$true)][string]$NssmPath,
  [Parameter(Mandatory=$true)][string]$InstallDir,
  [Parameter(Mandatory=$true)][string]$DatabaseUrl,
  [Parameter(Mandatory=$true)][string]$TelegramToken
)
$ErrorActionPreference = "Stop"
$InstallDir = (Resolve-Path $InstallDir).Path
$exe = Join-Path $InstallDir "bin\bvk.exe"
$config = Join-Path $InstallDir "config.json"
if (!(Test-Path $exe) -or !(Test-Path $config)) { throw "Build the application and create config.json first" }
& $NssmPath install BVKSupport $exe "-config" $config
if ($LASTEXITCODE -ne 0) { throw "Service installation failed" }
& $NssmPath set BVKSupport AppDirectory $InstallDir
& $NssmPath set BVKSupport AppEnvironmentExtra "DATABASE_URL=$DatabaseUrl" "TELEGRAM_BOT_TOKEN=$TelegramToken"
& $NssmPath set BVKSupport AppExit Default Restart
& $NssmPath set BVKSupport AppRestartDelay 5000
& $NssmPath set BVKSupport AppStopMethodConsole 30000
New-Item -ItemType Directory -Force (Join-Path $InstallDir "data\logs") | Out-Null
& $NssmPath set BVKSupport AppStdout (Join-Path $InstallDir "data\logs\service.log")
& $NssmPath set BVKSupport AppStderr (Join-Path $InstallDir "data\logs\error.log")
& $NssmPath set BVKSupport AppRotateFiles 1
& $NssmPath set BVKSupport AppRotateBytes 10485760
& $NssmPath set BVKSupport Start SERVICE_AUTO_START
Write-Host "Service installed. Set a dedicated account and restricted directory ACLs, run -check, then Start-Service BVKSupport."
