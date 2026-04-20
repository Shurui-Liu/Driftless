# experiments/exp2/run.ps1 - Experiment 2: Throughput & Latency vs Coordinator Cluster Size
#
# Run from the repo root:
#   $env:CLUSTER_SIZE      = "3"
#   $env:INGEST_URL        = "http://<ingest-ip>:8080/tasks"
#   $env:TASKS_TABLE       = "raft-coordinator-experiment-tasks"
#   $env:COORDINATOR_ADDRS = "http://10.2.11.74:8080,http://10.2.11.218:8080,http://10.2.11.174:8080"
#   .\experiments\exp2\run.ps1

param(
    [string]$ClusterSize      = $env:CLUSTER_SIZE,
    [string]$IngestUrl        = $env:INGEST_URL,
    [string]$TasksTable       = $env:TASKS_TABLE,
    [string]$CoordinatorAddrs = $env:COORDINATOR_ADDRS,
    [int[]] $Rates            = @(100, 500, 1000),
    [int]   $WarmupS          = 30,
    [int]   $MeasureS         = 120,
    [int]   $CooldownS        = 90
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if (-not $ClusterSize) { throw 'Set $env:CLUSTER_SIZE or pass -ClusterSize (3 or 5)' }
if (-not $IngestUrl)   { throw 'Set $env:INGEST_URL or pass -IngestUrl' }
if (-not $TasksTable)  { throw 'Set $env:TASKS_TABLE or pass -TasksTable' }

# Build load generator if needed
$LOADGEN = '.\bin\loadgen.exe'
if (-not (Test-Path $LOADGEN)) {
    Write-Host '[setup] Building load generator...' -ForegroundColor Yellow
    Push-Location experiments\load-generator
    go build -o ..\..\bin\loadgen.exe .
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'Failed to build loadgen' }
    Pop-Location
    Write-Host '[setup] loadgen built.' -ForegroundColor Green
}

# Install metrics collector deps
Write-Host '[setup] Installing metrics collector dependencies...' -ForegroundColor Yellow
pip install -q -r experiments\metrics-collector\requirements.txt
if ($LASTEXITCODE -ne 0) { throw 'pip install failed' }

# Prepare results file
$ResultsDir  = 'experiments\exp2\results'
New-Item -ItemType Directory -Force -Path $ResultsDir | Out-Null
$Timestamp   = Get-Date -Format 'yyyyMMdd_HHmmss'
$ResultsFile = $ResultsDir + '\' + $ClusterSize + 'node_' + $Timestamp + '.csv'

# Pre-compute duration strings (avoids variable-suffix parsing issues)
$DurWarmup  = $WarmupS.ToString()  + 's'
$DurMeasure = $MeasureS.ToString() + 's'
$LabelWarmup  = $WarmupS.ToString()  + 's'
$LabelMeasure = $MeasureS.ToString() + 's'
$LabelCool    = $CooldownS.ToString() + 's'

Write-Host ''
Write-Host '================================================================' -ForegroundColor Cyan
Write-Host (' Experiment 2 - ' + $ClusterSize + '-node coordinator cluster')  -ForegroundColor Cyan
Write-Host (' Rates : ' + ($Rates -join ', ') + ' tasks/min')                 -ForegroundColor Cyan
Write-Host (' Ingest: ' + $IngestUrl)                                          -ForegroundColor Cyan
Write-Host (' Output: ' + $ResultsFile)                                        -ForegroundColor Cyan
Write-Host '================================================================'  -ForegroundColor Cyan

foreach ($Rate in $Rates) {
    $Label = $ClusterSize + 'node_' + $Rate + 'tpm'
    Write-Host ''
    Write-Host ('--- Rate: ' + $Rate + ' tasks/min  (label: ' + $Label + ') ---') -ForegroundColor Yellow

    Write-Host ('  [1/3] warm-up (' + $LabelWarmup + ', not measured)...')
    & $LOADGEN -rate $Rate -duration $DurWarmup -url $IngestUrl | Out-Null

    Write-Host ('  [2/3] measuring (' + $LabelMeasure + ')...')
    $LoadgenOut = & $LOADGEN -rate $Rate -duration $DurMeasure -url $IngestUrl
    Write-Host ('        ' + $LoadgenOut)

    $Submitted  = if ($LoadgenOut -match 'submitted=(\d+)')      { $Matches[1] } else { '0' }
    $Failed     = if ($LoadgenOut -match 'failed=(\d+)')         { $Matches[1] } else { '0' }
    $ActualRate = if ($LoadgenOut -match 'actual_rate=([\d.]+)') { $Matches[1] } else { '0' }

    Write-Host ('  [3/3] draining queues (' + $LabelCool + ')...')
    Start-Sleep -Seconds $CooldownS

    Write-Host '  collecting metrics...'
    $CollectorArgs = @(
        'experiments\metrics-collector\collect.py',
        '--table',       $TasksTable,
        '--label',       $Label,
        '--submitted',   $Submitted,
        '--failed',      $Failed,
        '--actual-rate', $ActualRate,
        '--output',      $ResultsFile
    )
    if ($CoordinatorAddrs) { $CollectorArgs += @('--coordinator', $CoordinatorAddrs) }
    python @CollectorArgs
    if ($LASTEXITCODE -ne 0) { Write-Warning ('metrics collector exited with code ' + $LASTEXITCODE) }
}

Write-Host ''
Write-Host '================================================================' -ForegroundColor Green
Write-Host (' Experiment ' + $ClusterSize + '-node COMPLETE')                  -ForegroundColor Green
Write-Host (' Results: ' + $ResultsFile)                                       -ForegroundColor Green
if ($ClusterSize -eq '3') {
    Write-Host ''
    Write-Host ' Next - re-deploy 5-node and re-run:'                         -ForegroundColor Cyan
    Write-Host '   cd terraform\environments\experiment'                       -ForegroundColor Cyan
    Write-Host '   terraform apply -var-file 5node.tfvars'                    -ForegroundColor Cyan
    Write-Host '   $env:CLUSTER_SIZE = "5"'                                   -ForegroundColor Cyan
    Write-Host '   .\experiments\exp2\run.ps1'                                 -ForegroundColor Cyan
}
Write-Host '================================================================'  -ForegroundColor Green
