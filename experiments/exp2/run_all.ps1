# experiments/exp2/run_all.ps1  –  Experiment 2 full orchestration
#
# Collects 3-node results already in DynamoDB, then deploys a 5-node cluster,
# runs the same load sweep, and prints a side-by-side comparison.
#
# Run from the repo root:
#   .\experiments\exp2\run_all.ps1
#
# Resume flags (skip steps already done):
#   .\experiments\exp2\run_all.ps1 -Skip3NodeCollect
#   .\experiments\exp2\run_all.ps1 -Skip3NodeCollect -Skip5NodeDeploy -IngestUrl http://<ip>:8080/tasks

param(
    [string]$AwsRegion       = 'us-east-1',
    [string]$ClusterName     = 'raft-coordinator-experiment-cluster',
    [string]$IngestService   = 'raft-coordinator-experiment-ingest',
    [string]$TasksTable      = 'raft-coordinator-experiment-tasks',
    [string]$PeersTable      = 'raft-coordinator-experiment-peers',
    [int[]] $Rates           = @(100, 500, 1000),
    [int]   $WarmupS         = 30,
    [int]   $MeasureS        = 120,
    [int]   $CooldownS       = 90,

    # Already-completed 3-node loadgen numbers (from the run log)
    [int]   $N100Submitted   = 199,   [float]$N100Rate   = 99.5,
    [int]   $N500Submitted   = 998,   [float]$N500Rate   = 499.0,
    [int]   $N1000Submitted  = 1996,  [float]$N1000Rate  = 998.0,

    # Skip flags
    [switch]$Skip3NodeCollect,
    [switch]$Skip5NodeDeploy,
    [string]$IngestUrl = ''          # supply when skipping deploy
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$LOADGEN   = '.\bin\loadgen.exe'
$COLLECTOR = 'experiments\metrics-collector\collect.py'
$TF_DIR    = 'terraform\environments\experiment'
$OUT_3N    = 'experiments\exp2\results\3node_clean.csv'
$DurWarmup  = $WarmupS.ToString()  + 's'
$DurMeasure = $MeasureS.ToString() + 's'

# ── Helpers ───────────────────────────────────────────────────────────────────

function Ensure-LoadGen {
    if (Test-Path $LOADGEN) { return }
    Write-Host '[setup] Building load generator...' -ForegroundColor Yellow
    New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
    Push-Location experiments\load-generator
    go build -o ..\..\bin\loadgen.exe .
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'go build failed' }
    Pop-Location
    Write-Host '[setup] loadgen built.' -ForegroundColor Green
}

function Ensure-PipDeps {
    Write-Host '[setup] Installing Python deps...' -ForegroundColor Yellow
    pip install -q -r experiments\metrics-collector\requirements.txt
    if ($LASTEXITCODE -ne 0) { throw 'pip install failed' }
}

function Get-IngestPublicIP {
    Write-Host '[infra] Looking up ingest public IP...' -ForegroundColor Yellow

    $taskArn = (aws ecs list-tasks `
        --cluster       $ClusterName `
        --service-name  $IngestService `
        --region        $AwsRegion `
        --query         'taskArns[0]' `
        --output        text)
    if (-not $taskArn -or $taskArn -eq 'None') { throw 'No running ingest task found.' }

    $raw = aws ecs describe-tasks `
        --cluster $ClusterName --tasks $taskArn --region $AwsRegion | ConvertFrom-Json
    $eniId = ($raw.tasks[0].attachments[0].details |
              Where-Object { $_.name -eq 'networkInterfaceId' }).value
    if (-not $eniId) { throw 'Could not find ENI for ingest task.' }

    $ni = aws ec2 describe-network-interfaces `
        --network-interface-ids $eniId --region $AwsRegion | ConvertFrom-Json
    $ip = $ni.NetworkInterfaces[0].Association.PublicIp
    if (-not $ip -or $ip -eq 'None') { throw 'Ingest task has no public IP.' }
    return $ip
}

function Wait-RaftLeader {
    param([int]$TimeoutS = 180)
    Write-Host '[wait] Polling DynamoDB peers table for an elected leader...' -ForegroundColor Yellow
    $deadline = (Get-Date).AddSeconds($TimeoutS)
    while ((Get-Date) -lt $deadline) {
        try {
            $count = (aws dynamodb scan `
                --table-name    $PeersTable `
                --region        $AwsRegion `
                --filter-expression  'is_leader = :t' `
                --expression-attribute-values  '{\":t\":{\"BOOL\":true}}' `
                --select COUNT `
                --query  'Count' `
                --output text 2>$null)
            if ($count -and [int]$count -ge 1) {
                Write-Host '[wait] Leader found.' -ForegroundColor Green
                return
            }
        } catch { }
        Start-Sleep -Seconds 5
    }
    Write-Warning 'Timed out waiting for Raft leader — continuing anyway.'
}

function Invoke-Collect {
    param(
        [string]$Label,
        [int]   $Submitted,
        [int]   $Failed = 0,
        [float] $ActualRate,
        [string]$OutputFile,
        [string]$Since = '',
        [string]$Until = ''
    )
    $CollectorArgs = @(
        $COLLECTOR,
        '--region',      $AwsRegion,
        '--table',       $TasksTable,
        '--label',       $Label,
        '--submitted',   $Submitted,
        '--failed',      $Failed,
        '--actual-rate', $ActualRate,
        '--output',      $OutputFile
    )
    if ($Since) { $CollectorArgs += '--since'; $CollectorArgs += $Since }
    if ($Until) { $CollectorArgs += '--until'; $CollectorArgs += $Until }
    python @CollectorArgs
    if ($LASTEXITCODE -ne 0) { Write-Warning ('collect failed for ' + $Label + ' (exit ' + $LASTEXITCODE + ')') }
}

function Invoke-RateRun {
    param(
        [int]   $Rate,
        [string]$Url,
        [string]$Prefix,
        [string]$OutputFile
    )
    $Label = $Prefix + '_' + $Rate + 'tpm'
    Write-Host ''
    Write-Host ('--- Rate: ' + $Rate + ' tasks/min  (' + $Label + ') ---') -ForegroundColor Yellow

    Write-Host ('  [1/3] warm-up (' + $WarmupS + 's, not measured)...')
    & $LOADGEN -rate $Rate -duration $DurWarmup -url $Url | Out-Null

    $Since = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    Write-Host ('  [2/3] measuring (' + $MeasureS + 's)...')
    $LoadgenOut = & $LOADGEN -rate $Rate -duration $DurMeasure -url $Url
    $Until = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    Write-Host ('        ' + $LoadgenOut)

    $submitted  = if ($LoadgenOut -match 'submitted=(\d+)')      { [int]$Matches[1]   } else { 0 }
    $failed     = if ($LoadgenOut -match 'failed=(\d+)')         { [int]$Matches[1]   } else { 0 }
    $actualRate = if ($LoadgenOut -match 'actual_rate=([\d.]+)') { [float]$Matches[1] } else { 0.0 }

    Write-Host ('  [3/3] draining queues (' + $CooldownS + 's)...')
    Start-Sleep -Seconds $CooldownS

    Write-Host '  collecting metrics...'
    Invoke-Collect -Label $Label -Submitted $submitted -Failed $failed `
        -ActualRate $actualRate -OutputFile $OutputFile
}

# ── Setup ─────────────────────────────────────────────────────────────────────

Ensure-LoadGen
Ensure-PipDeps
New-Item -ItemType Directory -Force -Path 'experiments\exp2\results' | Out-Null

# ── Step 1: Collect 3-node results (all current DynamoDB data is 3-node) ──────

if (-not $Skip3NodeCollect) {
    Write-Host ''
    Write-Host '================================================================' -ForegroundColor Cyan
    Write-Host ' Step 1: Collecting 3-node results from DynamoDB'               -ForegroundColor Cyan
    Write-Host '================================================================' -ForegroundColor Cyan

    Invoke-Collect -Label '3node_100tpm'  -Submitted $N100Submitted  -ActualRate $N100Rate  -OutputFile $OUT_3N
    Invoke-Collect -Label '3node_500tpm'  -Submitted $N500Submitted  -ActualRate $N500Rate  -OutputFile $OUT_3N
    Invoke-Collect -Label '3node_1000tpm' -Submitted $N1000Submitted -ActualRate $N1000Rate -OutputFile $OUT_3N

    Write-Host ('[3-node] Saved to ' + $OUT_3N) -ForegroundColor Green
} else {
    Write-Host '[skip] 3-node collect.' -ForegroundColor DarkGray
}

# ── Step 2: Record split point before any 5-node tasks land ──────────────────

$FiveNodeStart = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
Write-Host ('[5-node] DynamoDB split boundary: ' + $FiveNodeStart) -ForegroundColor Cyan

# ── Step 3: Redeploy to 5-node ────────────────────────────────────────────────

if (-not $Skip5NodeDeploy) {
    Write-Host ''
    Write-Host '================================================================' -ForegroundColor Cyan
    Write-Host ' Step 2: Deploying 5-node coordinator cluster (Terraform)'      -ForegroundColor Cyan
    Write-Host '================================================================' -ForegroundColor Cyan

    Push-Location $TF_DIR
    terraform apply -var-file 5node.tfvars -auto-approve
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'terraform apply failed' }
    Pop-Location

    Write-Host '[deploy] Waiting 45s for ECS tasks to start...' -ForegroundColor Yellow
    Start-Sleep -Seconds 45
    Wait-RaftLeader -TimeoutS 180
} else {
    Write-Host '[skip] 5-node deploy.' -ForegroundColor DarkGray
}

# ── Step 4: Resolve ingest URL ────────────────────────────────────────────────

if (-not $IngestUrl) {
    $ip = Get-IngestPublicIP
    $IngestUrl = 'http://' + $ip + ':8080/tasks'
}
Write-Host ('[5-node] Ingest URL: ' + $IngestUrl) -ForegroundColor Green

# ── Step 5: Run 5-node load sweep ────────────────────────────────────────────

$Timestamp5N = Get-Date -Format 'yyyyMMdd_HHmmss'
$OUT_5N      = 'experiments\exp2\results\5node_' + $Timestamp5N + '.csv'

Write-Host ''
Write-Host '================================================================' -ForegroundColor Cyan
Write-Host ' Step 3: Running 5-node load sweep'                              -ForegroundColor Cyan
Write-Host (' Rates : ' + ($Rates -join ', ') + ' tasks/min')               -ForegroundColor Cyan
Write-Host (' Output: ' + $OUT_5N)                                           -ForegroundColor Cyan
Write-Host '================================================================' -ForegroundColor Cyan

foreach ($Rate in $Rates) {
    Invoke-RateRun -Rate $Rate -Url $IngestUrl -Prefix '5node' -OutputFile $OUT_5N
}

# ── Results ───────────────────────────────────────────────────────────────────

Write-Host ''
Write-Host '================================================================' -ForegroundColor Green
Write-Host ' Experiment 2 COMPLETE'                                          -ForegroundColor Green
Write-Host '================================================================' -ForegroundColor Green
Write-Host ''
Write-Host ' 3-node:' -ForegroundColor Cyan
if (Test-Path $OUT_3N) { Get-Content $OUT_3N }
Write-Host ''
Write-Host ' 5-node:' -ForegroundColor Cyan
if (Test-Path $OUT_5N) { Get-Content $OUT_5N }
Write-Host ''
Write-Host (' Files saved:')
Write-Host ('   ' + $OUT_3N)
Write-Host ('   ' + $OUT_5N)
Write-Host '================================================================' -ForegroundColor Green
