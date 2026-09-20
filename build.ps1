# Build ccodex-rotate for Windows and macOS into .\dist
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force -Path dist | Out-Null
$targets = @(@("darwin","arm64"), @("darwin","amd64"), @("windows","amd64"), @("windows","arm64"))
foreach ($t in $targets) {
  $os = $t[0]; $arch = $t[1]
  $out = "dist/ccodex-rotate-$os-$arch"
  if ($os -eq "windows") { $out += ".exe" }
  Write-Host "building $out"
  $env:GOOS = $os; $env:GOARCH = $arch; $env:CGO_ENABLED = "0"
  go build -trimpath -ldflags "-s -w" -o $out ./cmd/ccodex-rotate
}
Write-Host "done -> dist/"
