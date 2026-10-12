$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force bin | Out-Null
go test ./...
if ($LASTEXITCODE -ne 0) { throw "Tests failed" }
go build -trimpath -o bin/bvk.exe ./cmd/bvk
if ($LASTEXITCODE -ne 0) { throw "Build failed" }
