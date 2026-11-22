param(
    [string]$BaseUrl = 'http://localhost:10000',
    [string]$Duration = '10s',
    [string]$Hey = 'hey',
    [string]$Format = 'csv'
)
$ErrorActionPreference = 'Stop'
if ($Format -notin @('csv', 'jsonl')) { throw 'Format must be csv or jsonl' }
New-Item -ItemType Directory -Force tmp, reports | Out-Null
go build -o bin/datagen.exe ./cmd/datagen
if ($LASTEXITCODE -ne 0) { throw 'datagen build failed' }
& ./bin/datagen.exe --format $Format --bytes 1048576 --invalid-every 0 --output "tmp/load.$Format"
if ($LASTEXITCODE -ne 0) { throw 'fixture generation failed' }
$schema = (Get-Content examples/schema.json -Raw | ConvertFrom-Json | ConvertTo-Json -Depth 10 -Compress)
$headers = @('-H', "X-Schema: $schema")
if ($env:API_TOKEN) { $headers += @('-H', "Authorization: Bearer $env:API_TOKEN") }
$endpoint = $BaseUrl.TrimEnd('/') + '/api/validate?format=' + $Format
$health = Invoke-RestMethod ($BaseUrl.TrimEnd('/') + '/healthz')
if ($health.status -ne 'ok') { throw 'health check failed' }
foreach ($concurrency in @(1, 2, 8)) {
    $heyArgs = @('-z', $Duration, '-c', "$concurrency", '-t', '60', '-m', 'POST', '-T', 'application/octet-stream', '-D', "tmp/load.$Format") + $headers
    # hey's -q is per worker. Keep overload bounded to 200 requests/sec.
    if ($concurrency -eq 8) { $heyArgs += @('-q', '25') }
    $output = & $Hey @heyArgs $endpoint
    if ($LASTEXITCODE -ne 0) { throw "hey failed at concurrency $concurrency" }
    $output | Set-Content -Encoding utf8 "reports/hey-$Format-c$concurrency.txt"
    $output
}
Invoke-WebRequest ($BaseUrl.TrimEnd('/') + '/metrics') -OutFile "reports/metrics-after-$Format.txt"
Write-Host 'Compare successful throughput and p95. Count 429 separately; fast rejections inflate total RPS.'
