# experiments/exp3/run_all.ps1  –  Experiment 3: Exactly-Once Semantics Under Faults
#
# Submits tasks while injecting three concurrent fault types, then runs
# Porcupine linearizability analysis on the resulting history.
#
# Run from repo root:
#   .\experiments\exp3\run_all.ps1
#
# Resume after a completed run (analyze only):
#   .\experiments\exp3\run_all.ps1 -HistoryPath experiments\exp3\exp3-<timestamp>\history.jsonl

param(
    [string]$AwsRegion       = 'us-east-1',
    [string]$AccountId       = '471112634889',
    [string]$ClusterName     = 'raft-coordinator-experiment-cluster',
    [string]$TasksTable      = 'raft-coordinator-experiment-tasks',
    [string]$PeersTable      = 'raft-coordinator-experiment-peers',
    [string]$IngestQueueUrl  = 'https://sqs.us-east-1.amazonaws.com/471112634889/raft-coordinator-experiment-ingest',
    [string]$PayloadBucket   = 'raft-coordinator-experiment-task-data-471112634889',
    [string]$CoordServices   = 'raft-coordinator-experiment-coordinator-a,raft-coordinator-experiment-coordinator-b,raft-coordinator-experiment-coordinator-c,raft-coordinator-experiment-coordinator-d,raft-coordinator-experiment-coordinator-e',
    [string]$WorkerService   = 'raft-coordinator-experiment-worker',

    # Experiment parameters
    [int]   $TargetTasks          = 2000,
    [float] $RatePerMin           = 500.0,
    [float] $LeaderKillPeriod     = 30.0,
    [float] $FollowerKillPeriod   = 45.0,
    [float] $SqsRedeliverPeriod   = 20.0,

    # Skip to analyze step if run already completed
    [string]$HistoryPath = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$LINCHECK    = 'experiments\lincheck\lincheck.exe'
$HISTORY_DIR = (Resolve-Path 'experiments\exp3').Path
$RunName     = 'exp3-' + (Get-Date -Format 'yyyyMMddTHHmmss')

# ── Step 1: Build lincheck.exe ────────────────────────────────────────────────

if (-not (Test-Path $LINCHECK)) {
    Write-Host '[setup] Building lincheck.exe (Porcupine adapter)...' -ForegroundColor Yellow
    Push-Location experiments\lincheck
    go build -o lincheck.exe .
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'go build lincheck failed' }
    Pop-Location
    Write-Host '[setup] lincheck.exe built.' -ForegroundColor Green
}

# ── Step 2: Install Python dependencies ───────────────────────────────────────

Write-Host '[setup] Installing Python dependencies...' -ForegroundColor Yellow
pip install -q boto3 numpy matplotlib
if ($LASTEXITCODE -ne 0) { throw 'pip install failed' }

# ── Step 3: Set environment variables consumed by config.py ──────────────────

$env:AWS_REGION           = $AwsRegion
$env:AWS_ACCOUNT_ID       = $AccountId
$env:DRIFTLESS_ENV        = 'experiment'
$env:CLUSTER_NAME         = $ClusterName
$env:TASKS_TABLE          = $TasksTable
$env:PEERS_TABLE          = $PeersTable
$env:INGEST_QUEUE_URL     = $IngestQueueUrl
$env:PAYLOAD_BUCKET       = $PayloadBucket
$env:COORDINATOR_SERVICES = $CoordServices
$env:WORKER_SERVICE       = $WorkerService
$env:HISTORY_DIR          = $HISTORY_DIR

# ── Step 4: Run the experiment (submit + fault injection) ─────────────────────

$EstMin = [int]($TargetTasks / $RatePerMin)

Write-Host ''
Write-Host '================================================================' -ForegroundColor Cyan
Write-Host ' Experiment 3 — Exactly-Once Semantics Under Faults'            -ForegroundColor Cyan
Write-Host (' Tasks    : ' + $TargetTasks + ' @ ' + $RatePerMin + '/min (~' + $EstMin + ' min)') -ForegroundColor Cyan
Write-Host (' Faults   : leader kill /' + $LeaderKillPeriod + 's') -ForegroundColor Cyan
Write-Host ('            follower kill /' + $FollowerKillPeriod + 's') -ForegroundColor Cyan
Write-Host ('            SQS redeliver /' + $SqsRedeliverPeriod + 's') -ForegroundColor Cyan
Write-Host (' Run name : ' + $RunName)                                      -ForegroundColor Cyan
Write-Host (' Output   : experiments\exp3\' + $RunName + '\')              -ForegroundColor Cyan
Write-Host '================================================================' -ForegroundColor Cyan

if (-not $HistoryPath) {
    Write-Host ''
    Write-Host '[exp3] Starting run with fault injection...' -ForegroundColor Yellow
    Push-Location chaos-bench
    python exp3.py run `
        --tasks               $TargetTasks `
        --rate                $RatePerMin `
        --leader-kill-period  $LeaderKillPeriod `
        --follower-kill-period $FollowerKillPeriod `
        --sqs-redeliver-period $SqsRedeliverPeriod `
        --run-name            $RunName
    $RunExitCode = $LASTEXITCODE
    Pop-Location
    if ($RunExitCode -ne 0) { throw ('exp3 run failed (exit ' + $RunExitCode + ')') }

    $HistoryPath = $HISTORY_DIR + '\' + $RunName + '\history.jsonl'
} else {
    Write-Host ('[exp3] Skipping run, using existing history: ' + $HistoryPath) -ForegroundColor DarkGray
    $RunName = Split-Path (Split-Path $HistoryPath) -Leaf
}

if (-not (Test-Path $HistoryPath)) {
    throw ('History file not found: ' + $HistoryPath)
}

# ── Step 5: Analyze — dup scan + divergence + Porcupine ──────────────────────

Write-Host ''
Write-Host '[exp3] Running analysis (dup scan + divergence + Porcupine)...' -ForegroundColor Yellow
$LincheckAbs  = (Resolve-Path $LINCHECK).Path
$HistoryAbs   = (Resolve-Path $HistoryPath).Path
Push-Location chaos-bench
python exp3.py analyze $HistoryAbs --lincheck $LincheckAbs
$AnalyzeExitCode = $LASTEXITCODE
Pop-Location
if ($AnalyzeExitCode -ne 0) { Write-Warning ('exp3 analyze exited with code ' + $AnalyzeExitCode) }

# ── Step 6: Parse and display summary ────────────────────────────────────────

$RunDir      = Split-Path $HistoryAbs
$SummaryPath = $RunDir + '\exp3_summary.json'

if (-not (Test-Path $SummaryPath)) {
    Write-Warning 'exp3_summary.json not found — analysis may have failed.'
    exit 1
}

$s = Get-Content $SummaryPath | ConvertFrom-Json

$LinResult = if ($s.linearizability_violations -eq -1) { 'skipped' }
             elseif ($s.linearizability_violations -eq 0) { 'PASS (0 violations)' }
             else { 'FAIL (' + $s.linearizability_violations + ' violations)' }

$DupResult = if ($s.duplicate_dispatches -eq 0) { 'PASS (0 duplicates)' }
             else { 'FAIL (' + $s.duplicate_dispatches + ' duplicates, max receive_count=' + $s.max_receive_count + ')' }

Write-Host ''
Write-Host '================================================================' -ForegroundColor Green
Write-Host ' Experiment 3 COMPLETE'                                          -ForegroundColor Green
Write-Host '================================================================' -ForegroundColor Green
Write-Host ''
Write-Host (' Tasks submitted         : ' + $s.submitted)
Write-Host (' Tasks completed         : ' + $s.completed)
Write-Host (' Tasks timed out         : ' + $s.timed_out)
Write-Host (' Tasks lost              : ' + $s.lost)
Write-Host ''
Write-Host (' Faults injected:')
Write-Host ('   Leader kills          : ' + $s.leader_kills)
Write-Host ('   Follower kills        : ' + $s.follower_kills)
Write-Host ('   SQS redeliveries      : ' + $s.sqs_redeliveries_injected)
Write-Host ''
Write-Host (' Exactly-once checks:')
Write-Host ('   Duplicate dispatches  : ' + $DupResult)  -ForegroundColor $(if ($s.duplicate_dispatches -eq 0) { 'Green' } else { 'Red' })
Write-Host ('   Stuck tasks           : pending=' + $s.stuck_pending + '  assigned=' + $s.stuck_assigned)
Write-Host ('   Linearizability       : ' + $LinResult)  -ForegroundColor $(if ($s.linearizability_violations -eq 0) { 'Green' } elseif ($s.linearizability_violations -eq -1) { 'Yellow' } else { 'Red' })
Write-Host ''
Write-Host (' Output files:')
Write-Host ('   ' + $RunDir + '\history.jsonl')
Write-Host ('   ' + $RunDir + '\exp3_summary.json')
Write-Host ('   ' + $RunDir + '\exp3_dups.json')
Write-Host ('   ' + $RunDir + '\exp3_divergence.json')
Write-Host ('   ' + $RunDir + '\exp3_lincheck.json')
Write-Host '================================================================' -ForegroundColor Green
