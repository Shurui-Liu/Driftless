# experiments/exp2/run_all.ps1  –  Experiment 2 full orchestration
#
# Full clean re-run (recommended):
#   .\experiments\exp2\run_all.ps1 -Clean
#
# This will:
#   1. Deploy a fresh 3-node cluster via Terraform
#   2. Purge the DynamoDB tasks table and SQS ingest queue
#   3. Delete S3 Raft snapshots so nodes start at term=0 (prevents election storm)
#   4. Run the 3-node load sweep (100 / 500 / 1000 tpm)
#   5. Delete S3 snapshots again, then deploy a 5-node cluster
#   6. Run the 5-node load sweep
#   7. Collect per-window latency metrics and print a side-by-side comparison
#
# Resume flags (skip steps already done):
#   .\experiments\exp2\run_all.ps1 -Skip3NodeDeploy -Skip3NodeSweep -Skip5NodeDeploy -IngestUrl http://<ip>:8080/tasks

param(
    [string]$AwsRegion       = 'us-east-1',
    [string]$ClusterName     = 'raft-coordinator-experiment-cluster',
    [string]$IngestService   = 'raft-coordinator-experiment-ingest',
    [string]$TasksTable      = 'raft-coordinator-experiment-tasks',
    [string]$PeersTable      = 'raft-coordinator-experiment-peers',
    [string]$SnapshotBucket  = 'raft-coordinator-experiment-raft-snapshots-471112634889',
    [string]$IngestQueueUrl  = 'https://sqs.us-east-1.amazonaws.com/471112634889/raft-coordinator-experiment-ingest',
    [int[]] $Rates           = @(100, 500, 1000),
    [int]   $WarmupS         = 30,
    [int]   $MeasureS        = 120,
    [int]   $CooldownS       = 90,

    # Skip / resume flags
    [switch]$Clean,             # full clean re-run (purge table + queue + snapshots)
    [switch]$Skip3NodeDeploy,   # skip Terraform deploy of 3-node cluster
    [switch]$Skip3NodeSweep,    # skip 3-node load sweep (use existing DynamoDB data)
    [switch]$Skip5NodeDeploy,   # skip Terraform deploy of 5-node cluster
    [string]$IngestUrl = ''     # supply when skipping deploy steps
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$LOADGEN    = '.\bin\loadgen.exe'
$COLLECTOR  = 'experiments\metrics-collector\collect.py'
$TF_DIR     = 'terraform\environments\experiment'
$Timestamp  = Get-Date -Format 'yyyyMMdd_HHmmss'
$OUT_3N     = 'experiments\exp2\results\3node_' + $Timestamp + '.csv'
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

function Clear-RaftSnapshots {
    Write-Host '[snapshots] Deleting S3 Raft snapshots to force term=0 startup...' -ForegroundColor Yellow
    $keys = (aws s3api list-objects-v2 `
        --bucket $SnapshotBucket --prefix 'raft/' `
        --query 'Contents[].Key' --output text --region $AwsRegion 2>$null)
    if ($keys -and $keys -ne 'None') {
        foreach ($key in ($keys -split '\s+')) {
            if ($key) {
                aws s3 rm "s3://$SnapshotBucket/$key" --region $AwsRegion | Out-Null
                Write-Host ('  deleted s3://' + $SnapshotBucket + '/' + $key)
            }
        }
        Write-Host '[snapshots] Done.' -ForegroundColor Green
    } else {
        Write-Host '[snapshots] No snapshots found (already clean).' -ForegroundColor Green
    }
}

function Clear-DynamoTasks {
    Write-Host '[dynamo] Deleting all items from tasks table (via Python boto3)...' -ForegroundColor Yellow
    $deleted = python -c @"
import boto3, sys
region = '$AwsRegion'
table_name = '$TasksTable'
dynamo = boto3.resource('dynamodb', region_name=region)
table = dynamo.Table(table_name)
scan = table.scan(ProjectionExpression='task_id')
count = 0
with table.batch_writer() as batch:
    while True:
        for item in scan['Items']:
            batch.delete_item(Key={'task_id': item['task_id']})
            count += 1
        if 'LastEvaluatedKey' not in scan:
            break
        scan = table.scan(ProjectionExpression='task_id',
                          ExclusiveStartKey=scan['LastEvaluatedKey'])
print(count)
"@
    Write-Host ('[dynamo] Deleted ' + $deleted.Trim() + ' tasks.') -ForegroundColor Green
}

function Clear-SqsQueue {
    Write-Host '[sqs] Purging ingest queue...' -ForegroundColor Yellow
    aws sqs purge-queue --queue-url $IngestQueueUrl --region $AwsRegion | Out-Null
    Write-Host '[sqs] Purge initiated (takes up to 60s to complete).' -ForegroundColor Green
    Start-Sleep -Seconds 60
}

function Wait-RaftLeader {
    param([int]$TimeoutS = 180)
    Write-Host '[wait] Polling DynamoDB peers table for an elected leader...' -ForegroundColor Yellow
    $deadline = (Get-Date).AddSeconds($TimeoutS)
    while ((Get-Date) -lt $deadline) {
        try {
            # Scan without a filter-expression to avoid PowerShell 5.1 JSON
            # quote-stripping when passing --expression-attribute-values to
            # native executables.  Filter in PowerShell instead.
            $items = (aws dynamodb scan `
                --table-name $PeersTable `
                --region     $AwsRegion `
                --output     json 2>$null | ConvertFrom-Json).Items
            $leaders = $items | Where-Object { $_.is_leader.BOOL -eq $true }
            if ($leaders) {
                $leaderId = ($leaders | Select-Object -First 1).node_id.S
                Write-Host ('[wait] Leader found: ' + $leaderId) -ForegroundColor Green
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

    Write-Host ('  [window] since=' + $Since + '  until=' + $Until)
    Write-Host '  collecting metrics...'
    Invoke-Collect -Label $Label -Submitted $submitted -Failed $failed `
        -ActualRate $actualRate -OutputFile $OutputFile -Since $Since -Until $Until
}

# ── Setup ─────────────────────────────────────────────────────────────────────

Ensure-LoadGen
Ensure-PipDeps
New-Item -ItemType Directory -Force -Path 'experiments\exp2\results' | Out-Null

# ── Step 1: Deploy 3-node cluster ─────────────────────────────────────────────

if (-not $Skip3NodeDeploy) {
    Write-Host ''
    Write-Host '================================================================' -ForegroundColor Cyan
    Write-Host ' Step 1: Deploying 3-node coordinator cluster (Terraform)'     -ForegroundColor Cyan
    Write-Host '================================================================' -ForegroundColor Cyan

    if ($Clean) { Clear-RaftSnapshots }

    Push-Location $TF_DIR
    terraform apply -var-file 3node.tfvars -auto-approve
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'terraform apply failed' }
    Pop-Location

    Write-Host '[deploy] Waiting 60s for ECS tasks to start...' -ForegroundColor Yellow
    Start-Sleep -Seconds 60
    Wait-RaftLeader -TimeoutS 180
} else {
    Write-Host '[skip] 3-node deploy.' -ForegroundColor DarkGray
}

# ── Step 2: Purge stale data (clean run only) ─────────────────────────────────

if ($Clean) {
    Write-Host ''
    Write-Host '================================================================' -ForegroundColor Cyan
    Write-Host ' Step 2: Purging DynamoDB tasks table and SQS queue'           -ForegroundColor Cyan
    Write-Host '================================================================' -ForegroundColor Cyan
    Clear-DynamoTasks
    Clear-SqsQueue
} else {
    Write-Host '[skip] Data purge (pass -Clean to enable).' -ForegroundColor DarkGray
}

# ── Step 3: Run 3-node load sweep ─────────────────────────────────────────────

if (-not $Skip3NodeSweep) {
    if (-not $IngestUrl) {
        $ip = Get-IngestPublicIP
        $IngestUrl = 'http://' + $ip + ':8080/tasks'
    }
    Write-Host ('[3-node] Ingest URL: ' + $IngestUrl) -ForegroundColor Green

    Write-Host ''
    Write-Host '================================================================' -ForegroundColor Cyan
    Write-Host ' Step 3: Running 3-node load sweep'                            -ForegroundColor Cyan
    Write-Host (' Rates : ' + ($Rates -join ', ') + ' tasks/min')             -ForegroundColor Cyan
    Write-Host (' Output: ' + $OUT_3N)                                         -ForegroundColor Cyan
    Write-Host '================================================================' -ForegroundColor Cyan

    foreach ($Rate in $Rates) {
        Invoke-RateRun -Rate $Rate -Url $IngestUrl -Prefix '3node' -OutputFile $OUT_3N
    }
    Write-Host ('[3-node] Results saved to ' + $OUT_3N) -ForegroundColor Green
} else {
    Write-Host '[skip] 3-node sweep.' -ForegroundColor DarkGray
}

# ── Step 4: Deploy 5-node cluster ─────────────────────────────────────────────
# IMPORTANT: delete S3 snapshots before redeploying so all nodes start at
# term=0.  Without this, nodes that persisted high-term snapshots will cause
# a perpetual split-vote election storm on the new cluster.

if (-not $Skip5NodeDeploy) {
    Write-Host ''
    Write-Host '================================================================' -ForegroundColor Cyan
    Write-Host ' Step 4: Deploying 5-node coordinator cluster (Terraform)'     -ForegroundColor Cyan
    Write-Host '================================================================' -ForegroundColor Cyan

    Clear-RaftSnapshots   # always purge before 5-node deploy

    Push-Location $TF_DIR
    terraform apply -var-file 5node.tfvars -auto-approve
    if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'terraform apply failed' }
    Pop-Location

    Write-Host '[deploy] Waiting 60s for ECS tasks to start...' -ForegroundColor Yellow
    Start-Sleep -Seconds 60
    Wait-RaftLeader -TimeoutS 180
} else {
    Write-Host '[skip] 5-node deploy.' -ForegroundColor DarkGray
}

# ── Step 5: Run 5-node load sweep ─────────────────────────────────────────────

$OUT_5N = 'experiments\exp2\results\5node_' + $Timestamp + '.csv'

if (-not $IngestUrl) {
    $ip = Get-IngestPublicIP
    $IngestUrl = 'http://' + $ip + ':8080/tasks'
}
Write-Host ('[5-node] Ingest URL: ' + $IngestUrl) -ForegroundColor Green

Write-Host ''
Write-Host '================================================================' -ForegroundColor Cyan
Write-Host ' Step 5: Running 5-node load sweep'                              -ForegroundColor Cyan
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
Write-Host ' Files saved:'
Write-Host ('   ' + $OUT_3N)
Write-Host ('   ' + $OUT_5N)
Write-Host ''
Write-Host ' Generate graphs:'
Write-Host '   python experiments/exp2/plot_exp2.py'
Write-Host '================================================================' -ForegroundColor Green
