param(
    [string]$Config = (Join-Path $HOME 'config.yaml')
)

$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$configPath = (Resolve-Path -LiteralPath $Config).Path
$tempRoot = Join-Path ([IO.Path]::GetTempPath()) "spillway-client-check-$([guid]::NewGuid().ToString('N'))"
$tempHome = Join-Path $tempRoot 'home'
$spillway = Join-Path $tempRoot 'spillway.exe'
$oldUserProfile = $env:USERPROFILE
$oldHome = $env:HOME
$oldCodexHome = $env:CODEX_HOME

function Restore-EnvironmentVariable([string]$Name, [AllowNull()][string]$Value) {
    if ($null -eq $Value) {
        Remove-Item "Env:$Name" -ErrorAction SilentlyContinue
    } else {
        Set-Item "Env:$Name" $Value
    }
}

try {
    New-Item -ItemType Directory -Path $tempHome -Force | Out-Null

    Push-Location $repo
    try {
        go build -tags dashboardassets -o $spillway ./apps/spillway
        if ($LASTEXITCODE -ne 0) { throw "Spillway build failed (exit $LASTEXITCODE)." }

        & $spillway check -config $configPath *> $null
        if ($LASTEXITCODE -ne 0) { throw 'The selected config did not pass `spillway check`. No config contents were printed.' }

        go test -tags dashboardassets ./tests/cli ./tests/e2e -run 'Test(SetupClientsPreserveSettingsAndAvoidPrintingLocalKey|Binary_(ResponsesAndAnthropicClientsReachFakeProvider|SetupCodingClientsWritesPrivateGlobalConfig))' -count=1
        if ($LASTEXITCODE -ne 0) { throw "Client protocol and setup E2E tests failed (exit $LASTEXITCODE)." }

        $env:USERPROFILE = $tempHome
        $env:HOME = $tempHome
        $env:CODEX_HOME = Join-Path $tempHome '.codex'

        foreach ($client in @('claude', 'codex')) {
            & $spillway setup $client -config $configPath *> $null
            if ($LASTEXITCODE -ne 0) { throw "Spillway setup for $client failed. No setup output was printed." }
        }

        $claudeSettingsPath = Join-Path $tempHome '.claude\settings.json'
        $codexSettingsPath = Join-Path $env:CODEX_HOME 'spillway.config.toml'
        if (-not (Test-Path -LiteralPath $claudeSettingsPath -PathType Leaf)) { throw 'Claude Code settings were not created.' }
        if (-not (Test-Path -LiteralPath $codexSettingsPath -PathType Leaf)) { throw 'Codex profile was not created.' }

        $claudeSettings = Get-Content -LiteralPath $claudeSettingsPath -Raw | ConvertFrom-Json
        if ($claudeSettings.env.ANTHROPIC_BASE_URL -ne 'http://127.0.0.1:8080' -or
            $claudeSettings.env.ANTHROPIC_MODEL -ne 'auto' -or
            $claudeSettings.apiKeyHelper -notmatch 'spillway.*key.*default') {
            throw 'Claude Code settings do not point to the expected local Spillway endpoint and key helper.'
        }

        $codexSettings = Get-Content -LiteralPath $codexSettingsPath -Raw
        if ($codexSettings -notmatch 'model_provider = "spillway"' -or
            $codexSettings -notmatch 'base_url = "http://127\.0\.0\.1:8080/v1"' -or
            $codexSettings -notmatch 'wire_api = "responses"' -or
            $codexSettings -notmatch 'args = \["key", "-config", .+, "default"\]') {
            throw 'Codex profile does not point to the expected local Spillway Responses endpoint and key helper.'
        }

    } finally {
        Pop-Location
    }

    Write-Host 'PASS: config validation, isolated Claude Code and Codex setup, and local fake-provider E2E checks.'
    Write-Host 'No provider API calls were made; the generated client settings were temporary.'
} finally {
    Restore-EnvironmentVariable 'USERPROFILE' $oldUserProfile
    Restore-EnvironmentVariable 'HOME' $oldHome
    Restore-EnvironmentVariable 'CODEX_HOME' $oldCodexHome
    if (Test-Path -LiteralPath $tempRoot) {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force
    }
}
