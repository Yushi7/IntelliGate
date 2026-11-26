param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("round_robin", "adaptive")]
    [string]$Strategy,
    [Parameter(Mandatory = $true)]
    [string]$ProductBackendUrls,
    [Parameter(Mandatory = $true)]
    [string]$OrderBackendUrls,
    [Parameter(Mandatory = $true)]
    [string]$UserBackendUrls,
    [ValidateRange(1, 100000)]
    [int]$Users = 20,
    [ValidateRange(0.1, 100000)]
    [double]$SpawnRate = 5,
    [ValidatePattern("^\d+(s|m|h)$")]
    [string]$Duration = "5m",
    [ValidateRange(1, 86400)]
    [int]$WarmupSeconds = 30,
    [ValidateRange(0.000001, 1000)]
    [double]$RateLimitRate = 1000,
    [ValidateRange(0.000001, 1000)]
    [double]$RateLimitBurst = 1000,
    [ValidateSet("memory", "redis")]
    [string]$RateLimitStore = "memory",
    [string]$RateLimitRedisAddress = "localhost:6379",
    [ValidateRange(0, 2147483647)]
    [int]$RateLimitRedisDB = 0,
    [ValidateSet("closed", "open")]
    [string]$RateLimitFailureMode = "closed",
    [ValidatePattern("^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)$")]
    [string]$UpstreamTimeout = "5s",
    [ValidateRange(1, 2147483647)]
    [int]$BreakerFailureThreshold = 5,
    [ValidatePattern("^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)$")]
    [string]$BreakerOpenCooldown = "10s",
    [ValidateRange(1, 2147483647)]
    [int]$BreakerHalfOpenMaxRequests = 1,
    [ValidateRange(1, 2147483647)]
    [int]$BreakerHalfOpenSuccessThreshold = 1,
    [string]$GatewayUrl = "http://127.0.0.1:8080",
    [string]$PrometheusUrl = "http://127.0.0.1:9090"
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($env:INTELLIGATE_BENCHMARK_API_KEY)) {
    throw "Set INTELLIGATE_BENCHMARK_API_KEY in this PowerShell session."
}

function ConvertTo-BackendPool([string]$Service, [string]$Value) {
    $urls = @($Value.Split(',') | ForEach-Object { $_.Trim() })
    if ($urls.Count -eq 0 -or @($urls | Where-Object { [string]::IsNullOrWhiteSpace($_) }).Count -gt 0) {
        throw "The declared $Service backend URL list is empty or malformed."
    }
    $safeUrls = @()
    foreach ($backendUrl in $urls) {
        $parsed = $null
        if (-not [uri]::TryCreate($backendUrl, [UriKind]::Absolute, [ref]$parsed) -or
            $parsed.Scheme -notin @("http", "https") -or
            $parsed.UserInfo -or $parsed.Query -or $parsed.Fragment) {
            throw "The declared $Service backend URL list must contain absolute HTTP(S) URLs without user info, query strings, or fragments."
        }
        $safeUrls += $parsed.AbsoluteUri
    }
    return [ordered]@{ urls = $safeUrls; instance_count = $safeUrls.Count }
}

function ConvertTo-SafeHttpUrl([string]$Name, [string]$Value) {
    $parsed = $null
    if (-not [uri]::TryCreate($Value, [UriKind]::Absolute, [ref]$parsed) -or
        $parsed.Scheme -notin @("http", "https") -or
        $parsed.UserInfo -or $parsed.Query -or $parsed.Fragment) {
        throw "$Name must be an absolute HTTP(S) URL without user info, query strings, or fragments."
    }
    return $parsed.AbsoluteUri.TrimEnd("/")
}

function ConvertTo-SafeRedisAddress([string]$Value) {
    $parsed = $null
    if ([string]::IsNullOrWhiteSpace($Value) -or
        -not [uri]::TryCreate("redis://$Value", [UriKind]::Absolute, [ref]$parsed) -or
        $parsed.Scheme -ne "redis" -or -not $parsed.Host -or
        $parsed.Port -lt 1 -or $parsed.Port -gt 65535 -or
        $parsed.UserInfo -or $parsed.Query -or $parsed.Fragment -or $parsed.AbsolutePath -ne "/") {
        throw "RateLimitRedisAddress must be a host:port value without credentials or URL fields."
    }
    return $Value
}

function ConvertTo-DurationSeconds([string]$Value) {
    if ($Value -notmatch "^(?<amount>\d+)(?<unit>s|m|h)$") {
        throw "Duration must use integer seconds, minutes, or hours (for example 300s or 5m)."
    }
    $amount = [double]$Matches.amount
    switch ($Matches.unit) {
        "s" { return $amount }
        "m" { return $amount * 60 }
        "h" { return $amount * 3600 }
    }
}

function Assert-PositiveGatewayDuration([string]$Name, [string]$Value) {
    if ($Value -notmatch "^(?<amount>\d+(\.\d+)?)(?<unit>ns|us|µs|ms|s|m|h)$") {
        throw "$Name must use a positive Go duration such as 5s or 500ms."
    }
    $amount = [double]$Matches.amount
    if ($amount -le 0 -or [double]::IsNaN($amount) -or [double]::IsInfinity($amount)) {
        throw "$Name must use a positive finite Go duration."
    }
}

$requestedSeconds = ConvertTo-DurationSeconds $Duration
if ($requestedSeconds -lt 15) {
    throw "Measured duration must be at least 15 seconds."
}
if (($Users / $SpawnRate) + 2 -gt $WarmupSeconds) {
    throw "Warm-up must exceed the user spawn time by at least two seconds; increase WarmupSeconds or reduce Users."
}
$backendPools = [ordered]@{
    products = ConvertTo-BackendPool "products" $ProductBackendUrls
    orders = ConvertTo-BackendPool "orders" $OrderBackendUrls
    users = ConvertTo-BackendPool "users" $UserBackendUrls
}
$null = Assert-PositiveGatewayDuration "UpstreamTimeout" $UpstreamTimeout
$null = Assert-PositiveGatewayDuration "BreakerOpenCooldown" $BreakerOpenCooldown
$RateLimitRedisAddress = ConvertTo-SafeRedisAddress $RateLimitRedisAddress
$GatewayUrl = ConvertTo-SafeHttpUrl "GatewayUrl" $GatewayUrl
$PrometheusUrl = ConvertTo-SafeHttpUrl "PrometheusUrl" $PrometheusUrl

# Check the active interpreter and Locust before creating any benchmark output.
$pythonCommand = Get-Command python -ErrorAction SilentlyContinue
if ($null -eq $pythonCommand) {
    throw "Python was not found. Activate load-tests\.venv first."
}
$locustVersionOutput = & python -m locust --version 2>&1
if ($LASTEXITCODE -ne 0 -or $null -eq $locustVersionOutput) {
    throw "Locust is unavailable in the active Python environment. Install load-tests\requirements.txt before running a benchmark."
}
$locustVersion = ($locustVersionOutput | Out-String).Trim()
$locustFilePath = Join-Path $PSScriptRoot "locustfile.py"
$requirementsPath = Join-Path $PSScriptRoot "requirements.txt"
$locustFileHash = (Get-FileHash -LiteralPath $locustFilePath -Algorithm SHA256).Hash.ToLowerInvariant()
$requirementsHash = (Get-FileHash -LiteralPath $requirementsPath -Algorithm SHA256).Hash.ToLowerInvariant()
$runnerHash = (Get-FileHash -LiteralPath $PSCommandPath -Algorithm SHA256).Hash.ToLowerInvariant()

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$strategyQuery = "intelligate_routing_strategy_info{strategy=`"$Strategy`"}"
$queries = [ordered]@{
    gateway_scrape_up = 'up{job="intelligate-gateway"}'
    gateway_process_start_time = 'process_start_time_seconds{job="intelligate-gateway"}'
    selections = "sum by (service, backend_id, attempt) (intelligate_backend_selections_total)"
    backend_health = "intelligate_backend_health"
    rate_limit_rejections = "sum by (reason) (intelligate_rate_limit_rejections_total)"
    circuit_breaker = "intelligate_circuit_breaker_state"
    ewma_latency_seconds = "intelligate_backend_ewma_latency_seconds"
    in_flight_requests = "intelligate_backend_in_flight_requests"
    successful_samples = "intelligate_backend_successful_samples_total"
}

function Invoke-PrometheusQuery([string]$Query) {
    $encodedQuery = [uri]::EscapeDataString($Query)
    $uri = "$PrometheusUrl/api/v1/query?query=$encodedQuery"
    $response = Invoke-RestMethod -Method Get -Uri $uri -TimeoutSec 5
    if ($response.status -ne "success") {
        throw "Prometheus query did not return success."
    }
    return ,@($response.data.result)
}

function Get-MetricSnapshot {
    $snapshot = [ordered]@{
        captured_at_utc = [DateTime]::UtcNow.ToString("o")
        queries = [ordered]@{}
    }
    foreach ($name in $queries.Keys) {
        $snapshot.queries[$name] = Invoke-PrometheusQuery $queries[$name]
    }
    return $snapshot
}

function Save-MetricSnapshot([string]$Path) {
    $snapshot = Get-MetricSnapshot
    $snapshot | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $Path -Encoding utf8
    return $snapshot
}

function Assert-ExpectedBackendHealth($Snapshot) {
    $series = @($Snapshot.queries.backend_health)
    foreach ($service in @("products", "orders", "users")) {
        $expectedCount = $backendPools[$service].instance_count
        $serviceSeries = @($series | Where-Object { $_.metric.service -eq $service })
        if ($serviceSeries.Count -ne $expectedCount) {
            throw "Prometheus backend count for $service does not match the declared backend URL list."
        }
        foreach ($index in 1..$expectedCount) {
            $backendID = "$service-$index"
            $healthy = $serviceSeries | Where-Object {
                $_.metric.backend_id -eq $backendID -and [double]$_.value[1] -eq 1
            } | Select-Object -First 1
            if ($null -eq $healthy) {
                throw "Prometheus does not report declared backend $backendID as healthy."
            }
        }
    }
}

# Prometheus scrapes every five seconds; wait for the restarted gateway's fresh strategy.
$active = $null
$healthyScrape = $null
$deadline = [DateTime]::UtcNow.AddSeconds(30)
do {
    $nowSeconds = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() / 1000
    $scrape = Invoke-PrometheusQuery $queries.gateway_scrape_up
    $healthyScrape = $scrape | Where-Object {
        [double]$_.value[1] -eq 1 -and ($nowSeconds - [double]$_.value[0]) -le 15
    } | Select-Object -First 1
    $activeSeries = Invoke-PrometheusQuery $strategyQuery
    $active = $activeSeries | Where-Object {
        [double]$_.value[1] -eq 1 -and ($nowSeconds - [double]$_.value[0]) -le 15
    } | Select-Object -First 1
    if ($null -ne $healthyScrape -and $null -ne $active) { break }
    Start-Sleep -Seconds 2
} while ([DateTime]::UtcNow -lt $deadline)
if ($null -eq $healthyScrape -or $null -eq $active) {
    throw "Prometheus did not report a fresh healthy scrape with active strategy '$Strategy' within 30 seconds."
}

$runStamp = [DateTime]::UtcNow.ToString("yyyyMMddTHHmmssZ")
$runBaseName = "$Strategy-$runStamp"
$runDirectory = Join-Path $repoRoot "benchmark-results\phase-8\$runBaseName"
$runSuffix = 1
while (Test-Path -LiteralPath $runDirectory) {
    $runDirectory = Join-Path $repoRoot "benchmark-results\phase-8\$runBaseName-$runSuffix"
    $runSuffix++
}
$null = New-Item -ItemType Directory -Path $runDirectory
$gitRevision = (& git -C $repoRoot rev-parse HEAD 2>$null | Select-Object -First 1)
if ([string]::IsNullOrWhiteSpace($gitRevision)) {
    throw "Could not record the current Git revision."
}
$effectiveConfig = [ordered]@{
    source = "operator-supplied values mirrored from the gateway startup environment; the gateway does not expose these settings"
    backend_pools = $backendPools
    rate_limit = [ordered]@{
        enabled = $true
        rate_tokens_per_second = $RateLimitRate
        burst_tokens = $RateLimitBurst
        store = $RateLimitStore
        redis_address = $RateLimitRedisAddress
        redis_db = $RateLimitRedisDB
        failure_mode = $RateLimitFailureMode
    }
    retry = [ordered]@{
        max_retries = 2
        backoffs = @("100ms", "200ms")
        methods = @("GET", "HEAD")
        retry_budget = "3 x upstream timeout + 300ms"
        upstream_timeout = $UpstreamTimeout
    }
    upstream_timeout = $UpstreamTimeout
    circuit_breaker = [ordered]@{
        failure_threshold = $BreakerFailureThreshold
        open_cooldown = $BreakerOpenCooldown
        half_open_max_requests = $BreakerHalfOpenMaxRequests
        half_open_success_threshold = $BreakerHalfOpenSuccessThreshold
    }
}
$metadata = [ordered]@{
    status = "running"
    strategy = $Strategy
    gateway_url = $GatewayUrl.TrimEnd("/")
    prometheus_url = $PrometheusUrl.TrimEnd("/")
    users = $Users
    spawn_rate_users_per_second = $SpawnRate
    warmup_seconds = $WarmupSeconds
    measured_duration = $Duration
    requested_measured_seconds = $requestedSeconds
    started_at_utc = [DateTime]::UtcNow.ToString("o")
    git_revision = $gitRevision
    locust_version = $locustVersion
    locustfile_sha256 = $locustFileHash
    requirements_sha256 = $requirementsHash
    runner_sha256 = $runnerHash
    workload = "Equal-weight GET requests to products, orders, and users; 100-300ms wait per user."
    effective_gateway_config = $effectiveConfig
    api_key_source = "INTELLIGATE_BENCHMARK_API_KEY environment variable (not saved)"
}
$metadataPath = Join-Path $runDirectory "metadata.json"
$metadata | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $metadataPath -Encoding utf8
$failureStage = "warmup"
$failureCode = $null
$warmupElapsed = 0.0
$measurementElapsed = 0.0

try {
    $warmupClock = [Diagnostics.Stopwatch]::StartNew()
    & python -m locust -f $locustFilePath `
        --headless --only-summary --exit-code-on-error 0 `
        --host $GatewayUrl --users $Users --spawn-rate $SpawnRate `
        --run-time "${WarmupSeconds}s"
    $warmupCode = $LASTEXITCODE
    $warmupClock.Stop()
    $warmupElapsed = [math]::Round($warmupClock.Elapsed.TotalSeconds, 3)
    $metadata["warmup_elapsed_seconds"] = $warmupElapsed
    if ($warmupCode -ne 0 -or $warmupElapsed -lt ($WarmupSeconds - 2)) {
        $failureCode = $warmupCode
        throw "Warm-up did not complete."
    }
    $metadata["warmup_finished_at_utc"] = [DateTime]::UtcNow.ToString("o")

    $failureStage = "pre_measurement_metrics"
    $before = Save-MetricSnapshot (Join-Path $runDirectory "prometheus-before.json")
    Assert-ExpectedBackendHealth $before

    $failureStage = "measurement"
    $metadata["measurement_started_at_utc"] = [DateTime]::UtcNow.ToString("o")
    $measurementClock = [Diagnostics.Stopwatch]::StartNew()
    $locustPrefix = Join-Path $runDirectory "locust"
    & python -m locust -f $locustFilePath `
        --headless --only-summary --exit-code-on-error 0 `
        --host $GatewayUrl --users $Users --spawn-rate $SpawnRate `
        --run-time $Duration --csv $locustPrefix
    $measurementCode = $LASTEXITCODE
    $measurementClock.Stop()
    $measurementElapsed = [math]::Round($measurementClock.Elapsed.TotalSeconds, 3)
    $metadata["locust_exit_code"] = $measurementCode
    $metadata["measured_elapsed_seconds"] = $measurementElapsed
    $metadata["measurement_finished_at_utc"] = [DateTime]::UtcNow.ToString("o")

    $failureStage = "post_measurement_metrics"
    Start-Sleep -Seconds 6
    $after = Save-MetricSnapshot (Join-Path $runDirectory "prometheus-after.json")
    Assert-ExpectedBackendHealth $after

    $failureStage = "artifact_validation"
    if ($measurementCode -ne 0 -or $measurementElapsed -lt ($requestedSeconds - 2)) {
        $failureCode = $measurementCode
        throw "Measured Locust run ended early or failed."
    }
    $requiredFiles = @(
        "locust_stats.csv",
        "locust_stats_history.csv",
        "locust_failures.csv",
        "locust_exceptions.csv",
        "metadata.json",
        "prometheus-before.json",
        "prometheus-after.json"
    )
    foreach ($file in $requiredFiles) {
        $path = Join-Path $runDirectory $file
        if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or (Get-Item -LiteralPath $path).Length -eq 0) {
            throw "Required benchmark artifact is missing or empty."
        }
    }
    $stats = Import-Csv -LiteralPath (Join-Path $runDirectory "locust_stats.csv")
    $aggregate = $stats | Where-Object { $_.Name -eq "Aggregated" } | Select-Object -First 1
    $requiredColumns = @("Name", "Request Count", "Failure Count", "Requests/s", "Median Response Time", "Average Response Time", "95%", "99%")
    if ($null -eq $aggregate) {
        throw "Locust summary CSV does not contain the required aggregate columns."
    }
    $columns = @($stats[0].PSObject.Properties.Name)
    foreach ($column in $requiredColumns) {
        if ($column -notin $columns) { throw "Locust summary CSV does not contain the required aggregate columns." }
    }
    if ([long]$aggregate.'Request Count' -le 0 -or [long]$aggregate.'Failure Count' -gt [long]$aggregate.'Request Count') {
        throw "Locust summary CSV contains no requests or invalid failure counts."
    }

    $failureStage = "counter_validation"
    $beforeSelectionMap = @{}
    foreach ($series in @($before.queries.selections)) {
        $key = "$($series.metric.service)/$($series.metric.backend_id)/$($series.metric.attempt)"
        $value = [double]$series.value[1]
        if ($beforeSelectionMap.ContainsKey($key) -or $value -lt 0 -or
            [double]::IsNaN($value) -or [double]::IsInfinity($value) -or
            [math]::Abs($value - [math]::Round($value)) -gt 0.000001) {
            throw "Backend selection counters contain duplicate or invalid data."
        }
        $beforeSelectionMap[$key] = $value
    }
    $afterSelectionMap = @{}
    foreach ($series in @($after.queries.selections)) {
        $key = "$($series.metric.service)/$($series.metric.backend_id)/$($series.metric.attempt)"
        $value = [double]$series.value[1]
        if ($afterSelectionMap.ContainsKey($key) -or $value -lt 0 -or
            [double]::IsNaN($value) -or [double]::IsInfinity($value) -or
            [math]::Abs($value - [math]::Round($value)) -gt 0.000001) {
            throw "Backend selection counters contain duplicate or invalid data."
        }
        $afterSelectionMap[$key] = $value
    }
    $selectionCount = 0.0
    $selectionKeys = @(@($beforeSelectionMap.Keys) + @($afterSelectionMap.Keys) | Select-Object -Unique)
    foreach ($key in $selectionKeys) {
        $oldValue = if ($beforeSelectionMap.ContainsKey($key)) { $beforeSelectionMap[$key] } else { 0.0 }
        $newValue = if ($afterSelectionMap.ContainsKey($key)) { $afterSelectionMap[$key] } else { 0.0 }
        $delta = $newValue - $oldValue
        if ($delta -lt 0) { throw "Backend selection counter reset during measurement." }
        $selectionCount += $delta
    }
    if ($selectionCount -le 0) {
        throw "No backend selections were recorded during measurement."
    }
    $beforeStart = @($before.queries.gateway_process_start_time | ForEach-Object { [double]$_.value[1] })
    $afterStart = @($after.queries.gateway_process_start_time | ForEach-Object { [double]$_.value[1] })
    if ($beforeStart.Count -ne 1 -or $afterStart.Count -ne 1 -or $beforeStart[0] -ne $afterStart[0]) {
        throw "Gateway process restarted during measurement."
    }

    $metadata["backend_selection_delta_total"] = $selectionCount
    $metadata["status"] = "completed"
    $metadata["finished_at_utc"] = [DateTime]::UtcNow.ToString("o")
    $metadata | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $metadataPath -Encoding utf8
    $validationScript = "import sys; from pathlib import Path; sys.path.insert(0, sys.argv[1]); from compare_results import load_run; load_run(Path(sys.argv[2]))"
    & python -c $validationScript $PSScriptRoot $runDirectory
    if ($LASTEXITCODE -ne 0) {
        throw "Benchmark output validation failed."
    }
    $savedMetadata = Get-Content -LiteralPath $metadataPath -Raw | ConvertFrom-Json
    if ($savedMetadata.status -ne "completed" -or
        $savedMetadata.strategy -ne $Strategy -or
        $null -eq $savedMetadata.effective_gateway_config.backend_pools -or
        $null -eq $savedMetadata.measured_elapsed_seconds) {
        throw "Saved benchmark metadata failed validation."
    }
    Write-Host "Completed benchmark artifacts saved to $runDirectory"
} catch {
    $metadata["status"] = "failed"
    $metadata["failure_stage"] = $failureStage
    if ($null -ne $failureCode) { $metadata["locust_exit_code"] = $failureCode }
    $metadata["finished_at_utc"] = [DateTime]::UtcNow.ToString("o")
    $metadata | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $metadataPath -Encoding utf8
    throw "Benchmark failed during $failureStage. Partial artifacts were preserved at $runDirectory"
}
