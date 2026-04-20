# experiments/exp3/run.ps1 - Experiment 3: Exactly-Once Semantics Under Faults
#
# Run from the repo root:
#   .\experiments\exp3\run.ps1
#
# Override any default:
#   .\experiments\exp3\run.ps1 -TargetTasks 5000 -RatePerMin 300

param(
    [string]$AccountId          = '471112634889',
    [string]$AwsRegion          = 'us-east-1',
    [string]$ClusterName        = 'raft-coordinator-experiment-cluster',
    [string]$TasksTable         = 'raft-coordinator-experiment-tasks',
    [string]$PeersTable         = 'raft-coordinator-experiment-peers',
    [string]$IngestQueueUrl     = 'https://sqs.us-east-1.amazonaws.com/471112634889/raft-coordinator-experiment-ingest',
    [string]$CoordServices      = 'raft-coordinator-experiment-coordinator-a,raft-coordinator-experiment-coordinator-b,raft-coordinator-experiment-coordinator-c',
    [string]$WorkerService      = 'raft-coordinator-experiment-worker',
    [int]   $TargetTasks        = 10000,
    [int]   $RatePerMin         = 500,
    [float] $LeaderKillPeriod   = 30.0,
    [float] $FollowerKillPeriod = 45.0,
    [float] $SqsRedeliverPeriod = 20.0
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Build lincheck if needed
$LINCHECK = 'experiments\lincheck\lincheck.exe'
if (-not (Test-Path $LINCHECK)) {
    Write-Host '[setup] Building Porcupine lincheck binary...' -ForegroundColor Yellow
    Push-Location experiments\lincheck
    go build -o lincheck.exe .
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'Failed to build lincheck' }
    Pop-Location
    Write-Host '[setup] lincheck built.' -ForegroundColor Green
}

# Install Python dependencies
Write-Host '[setup] Installing chaos-bench Python dependencies...' -ForegroundColor Yellow
pip install -q -r chaos-bench\requirements.txt
if ($LASTEXITCODE -ne 0) { throw 'pip install failed' }

# Export env vars that chaos-bench/config.py reads
$RunName    = 'exp3-' + (Get-Date -Format 'yyyyMMddTHHmmssZ')
$HistoryDir = (Resolve-Path 'experiments\exp3').Path

$env:AWS_REGION           = $AwsRegion
$env:AWS_ACCOUNT_ID       = $AccountId
$env:DRIFTLESS_ENV        = 'experiment'
$env:CLUSTER_NAME         = $ClusterName
$env:TASKS_TABLE          = $TasksTable
$env:PEERS_TABLE          = $PeersTable
$env:INGEST_QUEUE_URL     = $IngestQueueUrl
$env:COORDINATOR_SERVICES = $CoordServices
$env:WORKER_SERVICE       = $WorkerService
$env:HISTORY_DIR          = $HistoryDir

# Pre-compute display strings
$EstMin    = [int]($TargetTasks / $RatePerMin)
$LKill     = $LeaderKillPeriod.ToString()   + 's'
$FKill     = $FollowerKillPeriod.ToString() + 's'
$SqsRdlvr  = $SqsRedeliverPeriod.ToString() + 's'

Write-Host ''
Write-Host '================================================================' -ForegroundColor Cyan
Write-Host ' Experiment 3 - Exactly-Once Semantics Under Faults'              -ForegroundColor Cyan
Write-Host (' Tasks : ' + $TargetTasks + ' @ ' + $RatePerMin + '/min (~' + $EstMin + ' min)') -ForegroundColor Cyan
Write-Host (' Faults: leader kill every ' + $LKill + ',')                     -ForegroundColor Cyan
Write-Host ('         follower kill every ' + $FKill + ',')                   -ForegroundColor Cyan
Write-Host ('         SQS redeliver every ' + $SqsRdlvr)                      -ForegroundColor Cyan
Write-Host (' Run   : ' + $RunName)                                            -ForegroundColor Cyan
Write-Host (' Output: experiments\exp3\' + $RunName + '\')                    -ForegroundColor Cyan
Write-Host '================================================================'  -ForegroundColor Cyan

# Run experiment
Push-Location chaos-bench

Write-Host ''
Write-Host '[exp3] Submitting tasks with fault injection...' -ForegroundColor Yellow
python exp3.py run `
    --tasks                 $TargetTasks `
    --rate                  $RatePerMin `
    --leader-kill-period    $LeaderKillPeriod `
    --follower-kill-period  $FollowerKillPeriod `
    --sqs-redeliver-period  $SqsRedeliverPeriod `
    --run-name              $RunName

if ($LASTEXITCODE -ne 0) { Pop-Location; throw ('exp3 run failed (exit ' + $LASTEXITCODE + ')') }

# Analyze
$HistoryPath = $HistoryDir + '\' + $RunName + '\history.jsonl'
if (-not (Test-Path $HistoryPath)) { Pop-Location; throw ('History not found: ' + $HistoryPath) }

Write-Host ''
Write-Host '[exp3] Analyzing (dup scan + divergence + Porcupine)...' -ForegroundColor Yellow
python exp3.py analyze $HistoryPath --lincheck ('..\' + $LINCHECK)

Pop-Location

# Print summary using PowerShell JSON parsing (no Python one-liners)
$SummaryPath = $HistoryDir + '\' + $RunName + '\exp3_summary.json'
if (Test-Path $SummaryPath) {
    $Summary = Get-Content $SummaryPath | ConvertFrom-Json
    Write-Host ''
    Write-Host '================================================================' -ForegroundColor Green
    Write-Host ' Experiment 3 COMPLETE - Summary'                               -ForegroundColor Green
    Write-Host '================================================================'  -ForegroundColor Green
    $Summary.PSObject.Properties | Where-Object { $_.Value -isnot [array] } | ForEach-Object {
        Write-Host ('  ' + $_.Name + ': ' + $_.Value)
    }
    Write-Host ''
    Write-Host (' Full results: experiments\exp3\' + $RunName + '\')           -ForegroundColor Green
    Write-Host '   exp3_summary.json     - all metrics'                         -ForegroundColor Green
    Write-Host '   exp3_dups.json        - duplicate dispatch detail'           -ForegroundColor Green
    Write-Host '   exp3_divergence.json  - stuck tasks'                         -ForegroundColor Green
    Write-Host '   exp3_lincheck.json    - Porcupine linearizability result'    -ForegroundColor Green
    Write-Host '================================================================'  -ForegroundColor Green
}
