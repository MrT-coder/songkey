# Cross-compiles the Linux binary so the server never has to run a Go toolchain.
# Run from the project root:  .\build.ps1

$ErrorActionPreference = 'Stop'

$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
$env:CGO_ENABLED = '0'

New-Item -ItemType Directory -Force -Path bin | Out-Null

Write-Host 'Running tests...' -ForegroundColor Cyan
$env:GOOS = ''; $env:GOARCH = ''
go test ./...
if ($LASTEXITCODE -ne 0) { throw 'tests failed' }

Write-Host 'Building linux/amd64 binary...' -ForegroundColor Cyan
$env:GOOS = 'linux'; $env:GOARCH = 'amd64'
go build -ldflags='-s -w' -o bin/songkey ./cmd/server
if ($LASTEXITCODE -ne 0) { throw 'build failed' }

$size = [math]::Round((Get-Item bin/songkey).Length / 1MB, 1)
Write-Host "OK - bin/songkey ($size MB)" -ForegroundColor Green
