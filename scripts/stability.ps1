param([string]$Model = 'deepseek/deepseek-v4-flash-free', [int]$N = 5, [int]$StreamN = 5)
$body = @{ model = $Model; messages = @(@{ role = 'user'; content = 'say ok in one word' }); max_tokens = 16 } | ConvertTo-Json -Compress
Set-Content "$env:TEMP\ds-body.json" -Value $body -Encoding ascii
$streamBody = @{ model = $Model; stream = $true; messages = @(@{ role = 'user'; content = 'say ok in one word' }); max_tokens = 16 } | ConvertTo-Json -Compress
Set-Content "$env:TEMP\ds-body-s.json" -Value $streamBody -Encoding ascii
$res = @()
function Do-Req($bodyFile, $label) {
    $t0 = Get-Date
    $r = curl.exe -s -w "`n%{http_code}" --max-time 120 -X POST 'http://127.0.0.1:9001/v1/chat/completions' -H "Authorization: Bearer admin-test-deepseek" -H 'Content-Type: application/json' --data-binary "@$bodyFile"
    $dt = ((Get-Date) - $t0).TotalMilliseconds
    $code = ($r -split "`n")[-1]
    $ok = ($r -match '"choices"') -and ($r -notmatch '"error"')
    if ($label -eq 'stream') { $ok = $ok -and ($r -match '\[DONE\]') }
    $err = ''
    if ($r -match '"message":"([^"]{0,90})"') { $err = $matches[1] }
    $script:res += [pscustomobject]@{ kind = $label; ms = [math]::Round($dt); code = $code; ok = $ok; err = $err }
    Write-Host ("  {0,-6} #{1}  {2}ms  http={3}  ok={4}  {5}" -f $label, $script:res.Count, [math]::Round($dt), $code, $ok, $err)
}
for ($i = 0; $i -lt $N; $i++) { Do-Req "$env:TEMP\ds-body.json" 'plain' }
for ($i = 0; $i -lt $StreamN; $i++) { Do-Req "$env:TEMP\ds-body-s.json" 'stream' }
$okN = @($res | Where-Object { $_.ok }).Count
$avg = [math]::Round(($res | Measure-Object ms -Average).Average)
$max = [math]::Round(($res | Measure-Object ms -Maximum).Maximum)
Write-Host ("RESULT: {0}/{1} ok | avg {2}ms | max {3}ms" -f $okN, $res.Count, $avg, $max)

