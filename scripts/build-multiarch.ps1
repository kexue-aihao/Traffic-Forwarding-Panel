param(
    [string]$Version = "dev"
)

$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path "dist" | Out-Null

$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -trimpath -ldflags="-s -w" -o "dist/trafficpanel-linux-amd64" ./cmd/trafficpanel

$env:GOARCH = "arm64"
go build -trimpath -ldflags="-s -w" -o "dist/trafficpanel-linux-arm64" ./cmd/trafficpanel

Write-Host "Built linux/amd64 and linux/arm64 binaries for $Version"

