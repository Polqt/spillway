<#+
.SYNOPSIS
Runs the v1.0 live routing evaluation pilot, then asks before the paid full run.

.DESCRIPTION
Prompts for the OpenRouter key without saving it, selects three models from
OpenRouter's current catalog, writes private run configs under .scratch, starts
three local Spillway servers, runs a stratified 10-prompt pilot, and estimates
whether the full run fits the $5 cap. The full run is opt-in at the prompt.
#>
[CmdletBinding()]
param(
    [string]$ClassifierModel = 'llama3.2:3b-instruct-q4_K_M',
    [int64]$Seed = 20261016
)

$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$outputRoot = Join-Path $repoRoot ('.scratch/v1.0-live-run-' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
$binaryPath = Join-Path $repoRoot 'bin/spillway.exe'
$ports = @{ auto = 18790; rules = 18791; classifier = 18792 }
$serverProcesses = [System.Collections.Generic.List[System.Diagnostics.Process]]::new()
$oldOpenRouterKey = $env:OPENROUTER_API_KEY
$oldEvalKey = $env:SPILLWAY_EVAL_KEY
$openRouterKey = $null

function Write-Step([string]$Message) {
    Write-Host "`n== $Message ==" -ForegroundColor Cyan
}

function Convert-SecureStringToPlainText([System.Security.SecureString]$Value) {
    $pointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($Value)
    try { [Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer) }
    finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer) }
}

function Get-OpenRouterModels([string]$Key) {
    try {
        $response = Invoke-RestMethod -Method Get -Uri 'https://openrouter.ai/api/v1/models' -Headers @{ Authorization = "Bearer $Key" } -TimeoutSec 30
        return @($response.data)
    } catch {
        throw 'Could not read the OpenRouter model catalog. Check the key and internet connection; the key was not printed or saved.'
    }
}

function Get-ModelById([object[]]$Catalog, [string]$Id) {
    $matches = @($Catalog | Where-Object { [string]$_.id -ceq $Id })
    if ($matches.Count -eq 0) { throw "Model id was not found in the current OpenRouter catalog: $Id" }
    $item = $matches[0]
    try {
        $inputPerMillion = [decimal]::Parse([string]$item.pricing.prompt, [Globalization.CultureInfo]::InvariantCulture) * 1000000
        $outputPerMillion = [decimal]::Parse([string]$item.pricing.completion, [Globalization.CultureInfo]::InvariantCulture) * 1000000
    } catch { throw "The catalog did not provide usable prices for model: $Id" }
    if ($inputPerMillion -lt 0 -or $outputPerMillion -lt 0) { throw "The catalog returned a negative price for model: $Id. Choose a model with non-negative input and output prices." }
    [pscustomobject]@{ Id = [string]$item.id; Input = $inputPerMillion; Output = $outputPerMillion }
}

function Select-CatalogModel([object[]]$Catalog, [string]$Label, [string]$Hint, [scriptblock]$Filter) {
    $candidates = @($Catalog | Where-Object $Filter | Select-Object -First 15)
    if ($candidates.Count -gt 0) {
        Write-Host "$Label examples from today's catalog ($Hint):"
        foreach ($candidate in $candidates) {
            $inputPrice = [decimal]::Parse([string]$candidate.pricing.prompt, [Globalization.CultureInfo]::InvariantCulture) * 1000000
            $outputPrice = [decimal]::Parse([string]$candidate.pricing.completion, [Globalization.CultureInfo]::InvariantCulture) * 1000000
            Write-Host ('  {0}  input ${1}/M, output ${2}/M' -f $candidate.id, $inputPrice, $outputPrice)
        }
    }
    while ($true) {
        $id = Read-Host "Enter the exact model id for $Label"
        try { return Get-ModelById $Catalog $id }
        catch { Write-Warning $_ }
    }
}

function Format-Price([decimal]$Value) {
    $Value.ToString('0.######', [Globalization.CultureInfo]::InvariantCulture)
}

function Write-RunConfig([string]$Path, [string]$Listen, [string]$Name, [object]$Free, [object]$Low, [object]$Strong, [string]$Classifier) {
    $freeRef = "openrouter/$($Free.Id)"
    $lowRef = "openrouter/$($Low.Id)"
    $strongRef = "openrouter/$($Strong.Id)"
    $classifierRef = "ollama/$Classifier"
    $router = ''
    if ($Name -ne 'auto') {
        # Each route has one OpenRouter model to prevent same-provider spillover on rate limits.
        $router = @'
router:
  mode: rules
  auto: [free, low, strong]
  difficulty:
    simple: [free]
    moderate: [low]
    hard: [strong]
'@
        if ($Name -eq 'classifier') { $router += "`n  classifier:`n    model: `"$classifierRef`"`n" }
    } else {
        $router = "router:`n  auto: [low]`n"
    }
    $dbPath = (Join-Path $outputRoot "store-$Name.db").Replace('\', '/')
    $yaml = @"
server:
  listen: "$Listen"
  api_keys: ["`${SPILLWAY_EVAL_KEY}"]
logging:
  level: warn
providers:
  openrouter:
    api_key: "`${OPENROUTER_API_KEY}"
  ollama:
    base_url: "http://localhost:11434"
tiers:
  free:
    models: ["$freeRef"]
  low:
    allow_spend: true
    models: ["$lowRef"]
  strong:
    allow_spend: true
    models: ["$strongRef"]
  classifier-model:
    models: ["$classifierRef"]
store:
  path: "$dbPath"
  retention: 2d
budgets:
  daily_usd: 1.66
pricing:
  "$freeRef": { input_usd_per_mtok: $(Format-Price $Free.Input), output_usd_per_mtok: $(Format-Price $Free.Output) }
  "$lowRef": { input_usd_per_mtok: $(Format-Price $Low.Input), output_usd_per_mtok: $(Format-Price $Low.Output) }
  "$strongRef": { input_usd_per_mtok: $(Format-Price $Strong.Input), output_usd_per_mtok: $(Format-Price $Strong.Output) }
$router
"@
    Set-Content -LiteralPath $Path -Value $yaml -Encoding utf8NoBOM
}

function Wait-ForPort([int]$Port, [System.Diagnostics.Process]$Process) {
    $deadline = (Get-Date).AddSeconds(45)
    while ((Get-Date) -lt $deadline) {
        if ($Process.HasExited) { throw "Spillway stopped while starting on port $Port. See the private log in $outputRoot." }
        $client = [Net.Sockets.TcpClient]::new()
        try {
            $pending = $client.BeginConnect('127.0.0.1', $Port, $null, $null)
            if ($pending.AsyncWaitHandle.WaitOne(300)) { $client.EndConnect($pending); return }
        } catch { }
        finally { $client.Dispose() }
        Start-Sleep -Milliseconds 250
    }
    throw "Spillway did not become ready on port $Port within 45 seconds. See the private log in $outputRoot."
}

function Invoke-Eval([string]$Dataset, [int]$Repeats, [string]$RunPath) {
    $arms = "current-auto=auto@http://127.0.0.1:$($ports.auto),rules=auto@http://127.0.0.1:$($ports.rules),classifier=auto@http://127.0.0.1:$($ports.classifier)"
    & $binaryPath eval -dataset $Dataset -api-key-env SPILLWAY_EVAL_KEY -arms $arms -repeats $Repeats -seed $Seed -out $RunPath
    if ($LASTEXITCODE -ne 0) { throw 'Spillway evaluation stopped with an error. No full-run artifacts were created; check the private logs and budget before retrying.' }
}

try {
    Write-Step 'Checking local prerequisites'
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Go is required. Install Go, reopen PowerShell, and run this script again.' }
    if (-not (Get-Command ollama -ErrorAction SilentlyContinue)) { throw 'Ollama is required. Install Ollama and the classifier model, then run this script again.' }
    $ollamaRows = @(& ollama list 2>$null | Select-Object -Skip 1)
    $ollamaNames = @($ollamaRows | ForEach-Object { ($_ -split '\s+')[0] })
    if ($LASTEXITCODE -ne 0 -or $ollamaNames -notcontains $ClassifierModel) { throw "Ollama model '$ClassifierModel' is not installed. Pass -ClassifierModel with the exact name shown by 'ollama list'." }
    try {
        $ollamaTags = Invoke-RestMethod -Method Get -Uri 'http://127.0.0.1:11434/api/tags' -TimeoutSec 10
        $ollamaDigest = [string](@($ollamaTags.models | Where-Object name -ceq $ClassifierModel | Select-Object -First 1).digest)
    } catch { throw 'Could not read the Ollama model digest from the local Ollama service.' }
    if ([string]::IsNullOrWhiteSpace($ollamaDigest)) { throw 'Could not find the selected model digest in Ollama.' }
    if (-not (Test-Path (Join-Path $repoRoot 'examples/evaluation-prompts.v2.json'))) { throw 'The v2 evaluation prompt set is missing.' }

    Write-Step 'Entering OpenRouter key'
    $secureKey = Read-Host 'Paste your OpenRouter key here (it will not be saved)' -AsSecureString
    $openRouterKey = Convert-SecureStringToPlainText $secureKey
    if ([string]::IsNullOrWhiteSpace($openRouterKey)) { throw 'An OpenRouter key is required.' }
    $env:OPENROUTER_API_KEY = $openRouterKey
    $keyBytes = [byte[]]::new(32)
    [Security.Cryptography.RandomNumberGenerator]::Fill($keyBytes)
    $env:SPILLWAY_EVAL_KEY = 'sk-spillway-eval-' + [Convert]::ToBase64String($keyBytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
    [Array]::Clear($keyBytes, 0, $keyBytes.Length)

    Write-Step 'Reading current OpenRouter model prices'
    $catalog = Get-OpenRouterModels $openRouterKey
    $freeModel = Select-CatalogModel $catalog 'free model' 'choose an eligible :free model' { $_.id.EndsWith(':free') }
    if (-not $freeModel.Id.EndsWith(':free') -or $freeModel.Input -ne 0 -or $freeModel.Output -ne 0) {
        throw 'The first model must be a zero-priced OpenRouter model whose id ends in :free. Start again and choose one of the free examples.'
    }
    $lowModel = Select-CatalogModel $catalog 'low-cost model' 'roughly $0.60/M blended or less' {
        -not $_.id.EndsWith(':free') -and
        [decimal]::Parse([string]$_.pricing.prompt, [Globalization.CultureInfo]::InvariantCulture) -ge 0 -and
        [decimal]::Parse([string]$_.pricing.completion, [Globalization.CultureInfo]::InvariantCulture) -ge 0 -and
        ([decimal]::Parse([string]$_.pricing.prompt, [Globalization.CultureInfo]::InvariantCulture) + [decimal]::Parse([string]$_.pricing.completion, [Globalization.CultureInfo]::InvariantCulture)) * 500000 -le 0.60
    }
    $strongModel = Select-CatalogModel $catalog 'strong model' 'frontier-class model; inspect the displayed price' {
        -not $_.id.EndsWith(':free') -and
        [decimal]::Parse([string]$_.pricing.prompt, [Globalization.CultureInfo]::InvariantCulture) -ge 0 -and
        [decimal]::Parse([string]$_.pricing.completion, [Globalization.CultureInfo]::InvariantCulture) -ge 0
    }
    if ($freeModel.Id -eq $lowModel.Id -or $freeModel.Id -eq $strongModel.Id -or $lowModel.Id -eq $strongModel.Id) { throw 'Choose three different model ids.' }
    if ($freeModel.Input -ne 0 -or $freeModel.Output -ne 0) { throw 'The selected :free model has a non-zero catalog price. Choose a zero-priced free model.' }
    if ($strongModel.Input -le $lowModel.Input -and $strongModel.Output -le $lowModel.Output) { throw 'Choose a stronger paid model priced above the low-cost model.' }

    Write-Step 'Preparing private run files'
    New-Item -ItemType Directory -Path $outputRoot -Force | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $outputRoot 'logs') -Force | Out-Null
    $configs = @{}
    foreach ($name in @('auto', 'rules', 'classifier')) {
        $port = $ports[$name]
        $configPath = Join-Path $outputRoot "config-$name.yaml"
        Write-RunConfig $configPath "127.0.0.1:$port" $name $freeModel $lowModel $strongModel $ClassifierModel
        $configs[$name] = $configPath
    }
    $priceRows = @{
        "openrouter/$($freeModel.Id)" = @{ input_usd_per_mtok = $freeModel.Input; output_usd_per_mtok = $freeModel.Output }
        "openrouter/$($lowModel.Id)" = @{ input_usd_per_mtok = $lowModel.Input; output_usd_per_mtok = $lowModel.Output }
        "openrouter/$($strongModel.Id)" = @{ input_usd_per_mtok = $strongModel.Input; output_usd_per_mtok = $strongModel.Output }
    }
    $priceRows | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $outputRoot 'candidate-prices.json') -Encoding utf8NoBOM
    @{
        run_started_local = (Get-Date).ToString('o')
        openrouter_catalog_date = (Get-Date).ToString('yyyy-MM-dd')
        classifier_model = $ClassifierModel
        classifier_digest = $ollamaDigest
        free_model = $freeModel.Id
        low_cost_model = $lowModel.Id
        strong_model = $strongModel.Id
        ollama_version = ((& ollama --version 2>&1 | Out-String).Trim())
        go_version = ((& go version 2>&1 | Out-String).Trim())
        seed = $Seed
        note = 'Keep this entire run directory private. It contains blinded completions and blind-key.json after the full run.'
    } | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $outputRoot 'run-info.json') -Encoding utf8NoBOM

    Push-Location $repoRoot
    try {
        & go build -o $binaryPath ./apps/spillway
        $buildExitCode = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    if ($buildExitCode -ne 0) { throw 'Could not build Spillway. Fix the build error before continuing.' }
    foreach ($name in @('auto', 'rules', 'classifier')) {
        & $binaryPath check -config $configs[$name] *> $null
        if ($LASTEXITCODE -ne 0) { throw "Generated $name config did not validate. No provider evaluation was started." }
    }
    & $binaryPath catalog -config $configs.classifier | Set-Content -LiteralPath (Join-Path $outputRoot 'catalog.yaml') -Encoding utf8NoBOM
    if ($LASTEXITCODE -ne 0) { throw 'Could not capture the Spillway catalog. No provider evaluation was started.' }

    Write-Step 'Starting three local Spillway processes'
    foreach ($name in @('auto', 'rules', 'classifier')) {
        $port = $ports[$name]
        $logPath = Join-Path $outputRoot "logs/$name.log"
        $process = Start-Process -FilePath $binaryPath -ArgumentList @('serve', '-config', ('"' + $configs[$name] + '"')) -WorkingDirectory $repoRoot -WindowStyle Hidden -PassThru -RedirectStandardOutput ($logPath + '.out') -RedirectStandardError ($logPath + '.err')
        $serverProcesses.Add($process)
        Wait-ForPort $port $process
        Write-Host "$name server is ready on 127.0.0.1:$port"
    }

    Write-Step 'Running the 10-prompt pilot'
    $fullDatasetPath = Join-Path $repoRoot 'examples/evaluation-prompts.v2.json'
    $promptSet = Get-Content -LiteralPath $fullDatasetPath -Raw | ConvertFrom-Json
    $pilotPrompts = @()
    foreach ($difficulty in @('simple', 'moderate', 'hard')) {
        $take = if ($difficulty -eq 'simple') { 4 } else { 3 }
        $pilotPrompts += @($promptSet.prompts | Where-Object difficulty -eq $difficulty | Select-Object -First $take)
    }
    if ($pilotPrompts.Count -ne 10) { throw 'Could not make a stratified 10-prompt pilot set.' }
    @{ version = $promptSet.version; prompts = $pilotPrompts } | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath (Join-Path $outputRoot 'pilot-prompts.json') -Encoding utf8NoBOM
    $pilotPath = Join-Path $outputRoot 'pilot'
    Invoke-Eval (Join-Path $outputRoot 'pilot-prompts.json') 1 $pilotPath
    $pilotMetrics = Get-Content -LiteralPath (Join-Path $pilotPath 'metrics.json') -Raw | ConvertFrom-Json
    $pilotCosts = @{}
    foreach ($arm in @('current-auto', 'rules', 'classifier')) {
        $pilotCosts[$arm] = [decimal](($pilotMetrics.trials | Where-Object arm -eq $arm | Measure-Object -Property cost_usd -Sum).Sum)
    }
    $pilotTotal = ($pilotCosts.Values | Measure-Object -Sum).Sum
    if ($pilotTotal -le 0) { throw 'Pilot returned no usable cost estimates. The paid full run is stopped to avoid exceeding the cap.' }
    $repeatCount = 3
    $projectedTotal = $pilotTotal + ($pilotTotal * 9 * $repeatCount)
    if ($projectedTotal -gt 4) {
        $repeatCount = 2
        $projectedTotal = $pilotTotal + ($pilotTotal * 9 * $repeatCount)
        Write-Warning 'The 3-repeat projection exceeds $4, so the protocol reduces the full run to 2 repeats.'
    }
    if ($projectedTotal -gt 5 -or (($pilotCosts.Values | ForEach-Object { $_ + ($_ * 9 * $repeatCount) } | Measure-Object -Maximum).Maximum -gt 1.60)) {
        throw ('The projected spend at 2 repeats is above the safe limit ($5 total or $1.60 per arm). Choose cheaper candidates and start a new run.')
    }

    Write-Host ('Pilot spend: ${0:N4}. Projected pilot plus full run: ${1:N4}, using {2} full-run repeats.' -f $pilotTotal, $projectedTotal, $repeatCount)
    Write-Host "Private run directory: $outputRoot"
    $confirm = Read-Host 'Run the paid 90-prompt evaluation now? Type RUN to continue'
    if ($confirm -cne 'RUN') { Write-Host 'Stopped after pilot. No full run was started.'; return }

    Write-Step "Running the full 90-prompt comparison ($repeatCount repeats)"
    $fullPath = Join-Path $outputRoot 'full'
    Invoke-Eval $fullDatasetPath $repeatCount $fullPath
    Copy-Item -LiteralPath (Join-Path $fullPath 'scores.csv') -Destination (Join-Path $fullPath 'rater-a.csv')
    Copy-Item -LiteralPath (Join-Path $fullPath 'scores.csv') -Destination (Join-Path $fullPath 'rater-b.csv')
    Write-Host "Completed. Private artifacts are in: $fullPath" -ForegroundColor Green
    Write-Host 'Do not share or commit answers.jsonl or blind-key.json. Two independent raters still need to score the blinded answers.'
} finally {
    foreach ($serverProcess in $serverProcesses) {
        try {
            if (-not $serverProcess.HasExited) {
                $serverProcess.Kill($true)
                $null = $serverProcess.WaitForExit(5000)
            }
        } catch {
            Write-Warning "Could not stop local Spillway process $($serverProcess.Id); stop it manually if it is still listening."
        } finally {
            $serverProcess.Dispose()
        }
    }
    if ($null -eq $oldOpenRouterKey) { Remove-Item Env:OPENROUTER_API_KEY -ErrorAction SilentlyContinue } else { $env:OPENROUTER_API_KEY = $oldOpenRouterKey }
    if ($null -eq $oldEvalKey) { Remove-Item Env:SPILLWAY_EVAL_KEY -ErrorAction SilentlyContinue } else { $env:SPILLWAY_EVAL_KEY = $oldEvalKey }
    if ($null -ne $openRouterKey) { $openRouterKey = $null }
}
