#Requires -Version 7.0
param(
    [switch]$RealModels,
    [string]$ClockDuration = '60s',
    [string]$EvidenceDirectory = (Join-Path $env:TEMP ('jobforge-validation-' + [guid]::NewGuid().ToString('N')))
)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$realModelURL = $env:JOBFORGE_REAL_MODEL_URL
$composeProject = $env:COMPOSE_PROJECT_NAME
$postgresPort = $env:JOBFORGE_POSTGRES_PORT
Push-Location -LiteralPath $projectRoot
try {
    New-Item -ItemType Directory -Path $EvidenceDirectory -Force | Out-Null
    $EvidenceDirectory = (Resolve-Path -LiteralPath $EvidenceDirectory).Path
    $python = Join-Path $projectRoot '.venv\Scripts\python.exe'
    if (-not (Test-Path -LiteralPath $python)) {
        throw 'Install .venv and tools/requirements-lint.txt as documented in docs/development.md first.'
    }
    if ($RealModels) {
        foreach ($name in @('JOBFORGE_REAL_MODEL_URL', 'JOBFORGE_TEST_OTLP_ENDPOINT', 'JOBFORGE_TEST_JAEGER_URL')) {
            if (-not [Environment]::GetEnvironmentVariable($name)) {
                throw "$name is required for real model acceptance; see docs/development.md."
            }
        }
    } else {
        # A quick run must not accidentally inherit a real-model endpoint.
        if ($env:JOBFORGE_REAL_MODEL_URL) {
            throw 'JOBFORGE_REAL_MODEL_URL is set; use -RealModels or unset it for the quick layer.'
        }
    }
    # Keep a demo shell's Compose overrides away from the disposable test DB.
    $env:COMPOSE_PROJECT_NAME = 'deploy'
    $env:JOBFORGE_POSTGRES_PORT = '5433'
    & docker compose -f deploy/compose.yaml up -d postgres
    if ($LASTEXITCODE -ne 0) { throw 'PostgreSQL bootstrap failed.' }
    & docker compose -f deploy/compose.yaml --profile durable-events up -d redis
    if ($LASTEXITCODE -ne 0) { throw 'Redis bootstrap failed.' }
    # This dedicated Compose database is disposable: TestMain rebuilds its schema.
    $env:JOBFORGE_TEST_DSN = 'postgres://jobforge:jobforge@localhost:5433/jobforge?sslmode=disable'
    $env:JOBFORGE_TEST_REDIS_URL = 'redis://localhost:6379/0'
    $env:JOBFORGE_TEST_REDIS_CONTAINER = 'deploy-redis-1'
    $env:JOBFORGE_TEST_PYTHON = $python

    & $python -m pip install --no-deps ./sdk/python
    if ($LASTEXITCODE -ne 0) { throw 'SDK wheel installation failed.' }
    & $python -m pytest sdk/python/tests
    if ($LASTEXITCODE -ne 0) { throw 'SDK tests failed.' }

    $clockExe = Join-Path $EvidenceDirectory 'clockcheck.exe'
    & go build -o $clockExe ./tools/clockcheck
    if ($LASTEXITCODE -ne 0) { throw 'Clock probe build failed.' }
    $clockJSON = & $clockExe -duration $ClockDuration
    $clockExit = $LASTEXITCODE
    $clockJSON | Set-Content -LiteralPath (Join-Path $EvidenceDirectory 'clock.json') -Encoding utf8
    if ($clockExit -ne 0) { throw 'Clock/SQL preflight failed. Read docs/runbooks/windows-acceptance.md; do not relax SLOs.' }
    $clockResult = $clockJSON | ConvertFrom-Json
    Write-Host ('Clock passed: max step {0:N3}ms; offset {1:N3}ms; SQL RTT {2:N3}ms' -f $clockResult.max_step_lower_bound_ms, $clockResult.max_offset_lower_bound_ms, $clockResult.max_rtt_ms)

    # Preserve the native exit code even when output is piped to Tee-Object.
    Remove-Item Env:JOBFORGE_REAL_MODEL_URL -ErrorAction SilentlyContinue
    & go test -race -count=1 -v ./... 2>&1 | Tee-Object -FilePath (Join-Path $EvidenceDirectory 'race.txt')
    $testExit = $LASTEXITCODE
    if ($testExit -ne 0) { throw "Race/integration acceptance failed (exit $testExit). Evidence: $EvidenceDirectory" }
    if ($RealModels) {
        $env:JOBFORGE_REAL_MODEL_URL = $realModelURL
        & go test -race -count=1 -v -run 'TestRealTasks' ./tests/integration/ 2>&1 | Tee-Object -FilePath (Join-Path $EvidenceDirectory 'real-models.txt')
        $testExit = $LASTEXITCODE
        if ($testExit -ne 0) { throw "Real model acceptance failed (exit $testExit). Evidence: $EvidenceDirectory" }
    }
    Write-Host "Acceptance passed. Evidence: $EvidenceDirectory"
    if (-not $RealModels) { Write-Host 'Real model tests were not requested; this does not accept the real model layer.' }
    Write-Host 'AT-25 remains skipped (P1 ControlStream). Historical W4, remote models and production retention remain unaccepted.'
} finally {
    $env:JOBFORGE_REAL_MODEL_URL = $realModelURL
    $env:COMPOSE_PROJECT_NAME = $composeProject
    $env:JOBFORGE_POSTGRES_PORT = $postgresPort
    Pop-Location
}
