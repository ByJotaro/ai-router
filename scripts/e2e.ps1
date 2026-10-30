# ai-router end-to-end verification: fake upstreams -> gateway -> hub.
# Run from the repo root:  powershell -ExecutionPolicy Bypass -File scripts\e2e.ps1
#
# Safety: only processes started by this script are stopped (by PID). It never
# kills processes it did not start (node/mimo/chrome/hub already running are
# left alone).
param(
    # Shared hub-module token. No default: a real token must never live in the
    # repo. Pass -HubModuleToken or set AI_ROUTER_HUB_TOKEN.
    [string]$HubModuleToken = $env:AI_ROUTER_HUB_TOKEN,
    # Path to hub.exe, needed only when no hub is already listening on :8700.
    [string]$HubExe = $env:AI_ROUTER_HUB_EXE,
    [int]$GatewayPort = 9101,
    [int]$HubmodPort  = 9102,
    [int]$FakeAPort   = 9321,
    [int]$FakeBPort   = 9322
)

if (-not $HubModuleToken) {
    throw 'HubModuleToken is required: pass -HubModuleToken <token> or set AI_ROUTER_HUB_TOKEN'
}

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$script:pass = 0
$script:fail = 0
$started = New-Object System.Collections.ArrayList  # PIDs we launched

function Check([string]$name, [bool]$ok, [string]$detail = '') {
    if ($ok) { $script:pass++; Write-Host "  PASS  $name" -ForegroundColor Green }
    else     { $script:fail++; Write-Host "  FAIL  $name  $detail" -ForegroundColor Red }
}

# Bodies are passed via --data-binary @file because PowerShell 5.1 strips
# embedded quotes from native-command arguments, corrupting inline JSON.
function Invoke-Curl([string]$method, [string]$url, [string]$bearer, [string]$body = '') {
    $args = @('-s', '--max-time', '20', '-X', $method, $url)
    if ($bearer) { $args += @('-H', "Authorization: Bearer $bearer") }
    if ($body) {
        $tmp = Join-Path $env:TEMP ("wr-body-" + [guid]::NewGuid().ToString('N').Substring(0, 8) + '.json')
        Set-Content -Path $tmp -Value $body -Encoding ascii -NoNewline
        $args += @('-H', 'Content-Type: application/json', '--data-binary', "@$tmp")
    }
    return (& curl.exe @args 2>$null)
}

function Stop-Started([int]$procId) {
    try { Stop-Process -Id $procId -Force -ErrorAction SilentlyContinue } catch {}
}

# --- Build ---------------------------------------------------------------
Write-Host '== Build ==' -ForegroundColor Cyan
go build -o "$root\dist\ai-router.exe" "$root\cmd\ai-router"
go build -o "$root\dist\fake-upstream.exe" "$root\cmd\fake-upstream"
Check 'go build ai-router/fake-upstream' ($LASTEXITCODE -eq 0)

function Test-Port([int]$port) {
    $c = New-Object System.Net.Sockets.TcpClient
    try { $c.Connect('127.0.0.1', $port); return $true } catch { return $false } finally { $c.Dispose() }
}

function Wait-Port([int]$port, [int]$timeoutS = 20) {
    $deadline = (Get-Date).AddSeconds($timeoutS)
    while ((Get-Date) -lt $deadline) {
        if (Test-Port $port) { return $true }
        Start-Sleep -Milliseconds 300
    }
    return $false
}

# --- Port preflight (never steal ports from running processes) ------------
foreach ($p in $GatewayPort, $HubmodPort, $FakeAPort, $FakeBPort) {
    if (Test-Port $p) {
        Write-Host "FAIL port $p already in use — aborting (no processes killed)" -ForegroundColor Red
        exit 1
    }
}

# --- Hub -----------------------------------------------------------------
$hub = 'http://127.0.0.1:8700'
$hubCfg = Join-Path $env:LOCALAPPDATA 'hub\config.yaml'
$hubExe = $HubExe
$hubWasRunning = Test-Port 8700

if (-not $hubWasRunning) {
    Write-Host '== Hub ==' -ForegroundColor Cyan
    if (-not (Test-Path $hubCfg)) { Write-Host "FAIL hub config missing: $hubCfg"; exit 1 }
    if (-not $hubExe) { Write-Host 'FAIL hub is not running on :8700 and -HubExe / AI_ROUTER_HUB_EXE is not set'; exit 1 }
    if (-not (Test-Path $hubExe)) { Write-Host "FAIL hub.exe missing: $hubExe"; exit 1 }
    # Pre-declare the ai-router module so its token authenticates immediately.
    $content = Get-Content $hubCfg -Raw
    if ($content -notmatch 'ai-router') {
        $block = "modules:`n    ai-router:`n        base_url: http://127.0.0.1:$HubmodPort`n        token: $HubModuleToken`n        auto_register: true"
        if ($content -match 'modules:\s*\{\}') {
            $content = $content -replace 'modules:\s*\{\}', $block
        } else {
            $content = $content.TrimEnd() + "`n" + $block
        }
        Set-Content -Path $hubCfg -Value $content -NoNewline
        Write-Host "  hub config: declared module ai-router"
    } elseif ($content -notmatch "base_url: http://127.0.0.1:$HubmodPort") {
        $content = $content -replace 'base_url: http://127.0.0.1:[0-9]+', "base_url: http://127.0.0.1:$HubmodPort"
        Set-Content -Path $hubCfg -Value $content -NoNewline
        Write-Host "  hub config: module base_url updated to 127.0.0.1:$HubmodPort"
    }
    $hubProc = Start-Process -FilePath $hubExe -WorkingDirectory (Split-Path $hubExe) -WindowStyle Hidden -PassThru
    # The hub is infrastructure — it stays running after the e2e; only the
    # gateway and fake upstreams are cleaned up.
    if (-not (Wait-Port 8700 25)) { Write-Host 'FAIL hub did not start'; Stop-Started $hubProc.Id; exit 1 }
    Write-Host "  hub started pid=$($hubProc.Id)"
} else {
    Write-Host '== Hub == (already running, left alone)' -ForegroundColor Cyan
}

$hubAdmin = $null
if (Test-Path $hubCfg) {
    if ((Get-Content $hubCfg -Raw) -match 'admin_token:\s*([0-9a-f]{64})') { $hubAdmin = $Matches[1] }
}
if (-not $hubAdmin) { Write-Host 'FAIL could not read hub admin token'; exit 1 }

# --- Fake upstreams -------------------------------------------------------
Write-Host '== Fake upstreams ==' -ForegroundColor Cyan
$fakeA = Start-Process -FilePath "$root\dist\fake-upstream.exe" -ArgumentList '-addr',"127.0.0.1:$FakeAPort",'-tag','A' -WindowStyle Hidden -PassThru
[void]$started.Add($fakeA.Id)
$fakeB = Start-Process -FilePath "$root\dist\fake-upstream.exe" -ArgumentList '-addr',"127.0.0.1:$FakeBPort",'-tag','B' -WindowStyle Hidden -PassThru
[void]$started.Add($fakeB.Id)
Check 'fake upstreams up' ((Wait-Port $FakeAPort 10) -and (Wait-Port $FakeBPort 10))

# --- ai-router config + start ---------------------------------------------
Write-Host '== ai-router ==' -ForegroundColor Cyan
# Удаляем возможную устаревшую регистрацию модуля (со старым base_url),
# иначе хаб отвечает «already registered» и модуль остаётся offline.
if ($hubAdmin) { $null = Invoke-Curl 'DELETE' "$hub/v1/modules/ai-router" $hubAdmin }
$work = Join-Path $env:TEMP ("ai-router-e2e-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Force -Path $work | Out-Null
$adminTok = 'e2e' + 'a' * 61
$vk = 'vk-e2e-' + [guid]::NewGuid().ToString('N').Substring(0, 12)
$cfgPath = Join-Path $work 'config.yaml'
@"
server:
    host: 127.0.0.1
    port: $GatewayPort
    log_level: debug
admin_token: $adminTok
hub:
    url: http://127.0.0.1:8700
    module: ai-router
    token: $HubModuleToken
    addr: 127.0.0.1:$HubmodPort
    register: true
providers:
    fake-a:
        type: openai-compatible
        base_url: http://127.0.0.1:$FakeAPort/v1
        api_keys: [upkey-a]
        models: [test-model, test-model-fail]
        fallbacks: [fake-b]
    fake-b:
        type: openai-compatible
        base_url: http://127.0.0.1:$FakeBPort/v1
        api_keys: [upkey-b]
        models: [test-model, test-model-fail]
keys:
    - name: e2e test key
      key: $vk
      quota_reqs: 2
"@ | Set-Content -Path $cfgPath -Encoding utf8

# e2e uses a unique virtual key, so persisted usage from earlier runs never
# trips the quota check.
$gateway = Start-Process -FilePath "$root\dist\ai-router.exe" -ArgumentList 'serve','-config',$cfgPath -RedirectStandardOutput (Join-Path $work 'out.log') -RedirectStandardError (Join-Path $work 'err.log') -WindowStyle Hidden -PassThru
[void]$started.Add($gateway.Id)
Check 'gateway up' (Wait-Port $GatewayPort 15)
$api = "http://127.0.0.1:$GatewayPort"

# --- OpenAI surface -------------------------------------------------------
# The virtual key (quota_reqs=2) is used only for the two consuming checks;
# everything else runs as admin so the quota check at the end stays valid.
Write-Host '== Gateway checks ==' -ForegroundColor Cyan
$models = Invoke-Curl 'GET' "$api/v1/models" $adminTok
Check '/v1/models lists test-model' ($models -match 'test-model')

$chatBody = '{"model":"test-model","messages":[{"role":"user","content":"hi"}]}'
$resp = Invoke-Curl 'POST' "$api/v1/chat/completions" $vk $chatBody
Check 'chat non-stream ok (fake-A)' ($resp -match '"hello from fake-A"')

$stream = Invoke-Curl 'POST' "$api/v1/chat/completions" $vk '{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}'
Check 'stream: chunks + usage + [DONE]' (($stream -match 'data: ') -and ($stream -match '"total_tokens":8') -and ($stream -match '\[DONE\]'))

$fb = Invoke-Curl 'POST' "$api/v1/chat/completions" $adminTok '{"model":"test-model-fail","messages":[{"role":"user","content":"hi"}]}'
Check 'fallback: fake-A 500 -> fake-B' ($fb -match '"hello from fake-B"')

$toolsBody = '{"model":"test-model","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{}}}}]}'
$tools = Invoke-Curl 'POST' "$api/v1/chat/completions" $adminTok $toolsBody
Check 'tools -> tool_calls in response' ($tools -match 'tool_calls')

$badKey = Invoke-Curl 'POST' "$api/v1/chat/completions" 'wrong-key' $chatBody
Check 'invalid virtual key -> 401' ($badKey -match '"unauthorized"')

$badModel = Invoke-Curl 'POST' "$api/v1/chat/completions" $adminTok '{"model":"no-such-model","messages":[]}'
Check 'unknown model -> model_not_found' ($badModel -match 'model_not_found')

$quota = Invoke-Curl 'POST' "$api/v1/chat/completions" $vk $chatBody
Check 'quota exhausted -> 429' (($quota -match 'rate_limit') -or ($quota -match '429'))

# --- Hub integration ------------------------------------------------------
Write-Host '== Hub integration ==' -ForegroundColor Cyan
$modules = Invoke-Curl 'GET' "$hub/v1/modules" $hubAdmin
Check 'hub sees ai-router' ($modules -match 'ai-router')

$online = $false
for ($i = 0; $i -lt 30; $i++) {
    Start-Sleep -Seconds 1
    $m = Invoke-Curl 'GET' "$hub/v1/modules/ai-router" $hubAdmin
    if ($m -match '"state":"online"') { $online = $true; break }
}
Check 'ai-router online in hub' $online

$call = Invoke-Curl 'POST' "$hub/v1/call/ai-router/chat.complete" $hubAdmin '{"params":{"model":"test-model","messages":[{"role":"user","content":"hello hub"}]}}'
$callDetail = if ($call) { $call.Substring(0, [Math]::Min(200, $call.Length)) } else { '' }
Check 'hub call chat.complete' ($call -match 'hello from fake-A')  $callDetail

$test = Invoke-Curl 'POST' "$hub/v1/call/ai-router/test" $hubAdmin '{"params":{}}'
Check 'hub call test' ($test -match '"ok":true')

$usage = Invoke-Curl 'POST' "$hub/v1/call/ai-router/usage" $hubAdmin '{"params":{}}'
Check 'hub call usage' ($usage -match 'total_requests')

# --- Cleanup (only PIDs we started) ----------------------------------------
Write-Host '== Cleanup ==' -ForegroundColor Cyan
foreach ($p in $started) { Stop-Started $p }
Start-Sleep -Milliseconds 500

Write-Host ''
Write-Host "RESULT: $script:pass passed, $script:fail failed" -ForegroundColor Cyan
if ($script:fail -gt 0) { exit 1 }
