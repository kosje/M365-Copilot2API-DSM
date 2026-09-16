# M365 Copilot2API 测试记录器
# 用途：自动化测试各端点，记录请求/响应/延迟/错误，便于回归测试

param(
    [string]$BaseUrl = "http://127.0.0.1:4141",
    [string]$LogFile = "test-results.jsonl",
    [string]$AdminPassword = $env:ADMIN_PASSWORD,
    # New administrator password used by the change-password cases. There is
    # deliberately NO built-in default: this value is written to the target.
    [string]$NewAdminPassword = $env:M365_TEST_NEW_ADMIN_PW,
    # Required to run against anything other than loopback.
    [switch]$AllowRemoteMutations
)

# ---------------------------------------------------------------------------
# 安全护栏：本脚本会改动目标实例的状态（修改管理员口令、新建/删除 API Key 等）。
# 因此：
#   1) 目标默认为回环地址；指向非回环地址必须显式加 -AllowRemoteMutations；
#   2) 管理员新口令没有内置默认值，必须由调用方提供。
# 历史版本在此内置了一个固定口令，等于把目标口令改成一个人人皆知的值。
# ---------------------------------------------------------------------------
$isLoopbackTarget = $BaseUrl -match '^https?://(127\.0\.0\.1|localhost|\[::1\])(:\d+)?(/.*)?$'
if (-not $isLoopbackTarget -and -not $AllowRemoteMutations) {
    Write-Host "[ABORT] 目标 $BaseUrl 不是回环地址，而本脚本会改动目标数据。" -ForegroundColor Red
    Write-Host "        确认该目标可以随意改动后，加 -AllowRemoteMutations 重新运行。" -ForegroundColor Red
    exit 1
}
$newPassword = $NewAdminPassword
if ([string]::IsNullOrWhiteSpace($newPassword)) {
    Write-Host "[ABORT] 未提供管理员新口令。" -ForegroundColor Red
    Write-Host "        请设置 `$env:M365_TEST_NEW_ADMIN_PW（或传 -NewAdminPassword），" -ForegroundColor Red
    Write-Host "        口令需 >=12 位且至少包含大小写字母、数字、符号中的三类。" -ForegroundColor Red
    exit 1
}

$results = [System.Collections.ArrayList]::new()
$timestamp = Get-Date -Format "yyyy-MM-dd HH:mm:ss"

function Invoke-Test {
    param(
        [string]$Name,
        [scriptblock]$Action
    )
    Write-Host "[TEST] $Name" -ForegroundColor Cyan
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        $result = & $Action
        $sw.Stop()
        $record = @{
            timestamp = $timestamp
            name = $Name
            status = "PASS"
            duration_ms = $sw.ElapsedMilliseconds
            detail = $result
            error = $null
        }
        Write-Host "  -> PASS ($($sw.ElapsedMilliseconds)ms)" -ForegroundColor Green
    }
    catch {
        $sw.Stop()
        $record = @{
            timestamp = $timestamp
            name = $Name
            status = "FAIL"
            duration_ms = $sw.ElapsedMilliseconds
            detail = $null
            error = $_.Exception.Message
        }
        Write-Host "  -> FAIL ($($sw.ElapsedMilliseconds)ms): $($_.Exception.Message)" -ForegroundColor Red
    }
    $null = $results.Add($record)
    $record | ConvertTo-Json -Compress | Out-File -Append -FilePath $LogFile -Encoding UTF8
    return $record
}

Write-Host "`n=== M365 Copilot2API 测试 ===" -ForegroundColor Yellow
Write-Host "Target: $BaseUrl" -ForegroundColor Gray
Write-Host ""

# === 1. 根路径 ===
Invoke-Test "GET / (根路径)" {
    $r = Invoke-WebRequest -Uri "$BaseUrl/" -TimeoutSec 10 -UseBasicParsing
    if ($r.StatusCode -ne 200) { throw "HTTP $($r.StatusCode)" }
    return @{ status = $r.StatusCode; length = $r.Content.Length }
}

# === 2. 根路径/管理后台 ===
Invoke-Test "GET / (管理后台)" {
    $r = Invoke-WebRequest -Uri "$BaseUrl/" -TimeoutSec 10 -UseBasicParsing
    if ($r.StatusCode -ne 200) { throw "HTTP $($r.StatusCode)" }
    return @{ status = $r.StatusCode; length = $r.Content.Length }
}

# === 4. API Keys 未认证 ===
Invoke-Test "GET /api/admin/keys (未认证)" {
    try {
        $r = Invoke-WebRequest -Uri "$BaseUrl/api/admin/keys" -TimeoutSec 10 -UseBasicParsing
        return @{ status = $r.StatusCode; body = $r.Content.Substring(0, [Math]::Min(200, $r.Content.Length)) }
    } catch {
        return @{ status = $_.Exception.Response.StatusCode.value__; error = $_.Exception.Message }
    }
}

# === 5. Chat Completions 无 Key ===
Invoke-Test "POST /v1/chat/completions (无API Key)" {
    $body = @{ model = "gpt-5.5"; messages = @(@{role="user";content="hi"}) } | ConvertTo-Json
    try {
        $r = Invoke-WebRequest -Uri "$BaseUrl/v1/chat/completions" -Method POST -ContentType "application/json" -Body $body -TimeoutSec 10 -UseBasicParsing
        return @{ status = $r.StatusCode }
    } catch {
        return @{ status = $_.Exception.Response.StatusCode.value__; error = $_.Exception.Message }
    }
}

# === 6. 管理员认证（自动处理密码修改）===
$adminToken = $null
# $newPassword is validated by the guard at the top of this script.
function Invoke-AdminLogin {
    param([string]$pwd)
    $body = @{ password = $pwd } | ConvertTo-Json
    $r = Invoke-WebRequest -Uri "$BaseUrl/api/admin/login" -Method POST -ContentType "application/json" -Body $body -TimeoutSec 10 -UseBasicParsing -SessionVariable sess
    return @{ response = $r; session = $sess }
}

Invoke-Test "POST /api/admin/login (管理员登录)" {
    $loginResult = Invoke-AdminLogin -pwd $AdminPassword
    if ($loginResult.response.StatusCode -ne 200) {
        # 可能密码已被修改，尝试新密码
        $loginResult = Invoke-AdminLogin -pwd $newPassword
        if ($loginResult.response.StatusCode -ne 200) { throw "HTTP $($loginResult.response.StatusCode)" }
    }
    $script:adminToken = $loginResult.session
    $respBody = $loginResult.response.Content | ConvertFrom-Json
    return @{ status = $loginResult.response.StatusCode; must_change = $respBody.must_change_password }
}

# === 7. 修改默认密码（如需要）===
Invoke-Test "POST /api/admin/change-password (修改默认密码)" {
    $body = @{ current_password = $AdminPassword; new_password = $newPassword } | ConvertTo-Json
    try {
        $r = Invoke-WebRequest -Uri "$BaseUrl/api/admin/change-password" -Method POST -ContentType "application/json" -Body $body -TimeoutSec 10 -UseBasicParsing -WebSession $adminToken
        return @{ status = $r.StatusCode }
    } catch {
        $statusCode = $_.Exception.Response.StatusCode.value__
        if ($statusCode -eq 401) {
            return @{ status = "SKIPPED"; reason = "Password already changed" }
        }
        throw $_.Exception.Message
    }
}

# === 8. 重新登录（密码修改后session已失效）===
Invoke-Test "POST /api/admin/login (重新登录)" {
    $body = @{ password = $newPassword } | ConvertTo-Json
    $r = Invoke-WebRequest -Uri "$BaseUrl/api/admin/login" -Method POST -ContentType "application/json" -Body $body -TimeoutSec 10 -UseBasicParsing -SessionVariable sess
    if ($r.StatusCode -ne 200) { throw "HTTP $($r.StatusCode)" }
    $script:adminToken = $sess
    return @{ status = $r.StatusCode; cookies = $sess.Cookies.Count }
}

# === 9. 创建 API Key ===
$createdApiKey = $null
Invoke-Test "POST /api/admin/keys (创建API Key)" {
    $body = @{ name = "test-key-$(Get-Random)" } | ConvertTo-Json
    $r = Invoke-WebRequest -Uri "$BaseUrl/api/admin/keys" -Method POST -ContentType "application/json" -Body $body -TimeoutSec 10 -UseBasicParsing -WebSession $adminToken
    if ($r.StatusCode -ne 200 -and $r.StatusCode -ne 201) { throw "HTTP $($r.StatusCode)" }
    $resp = $r.Content | ConvertFrom-Json
    $script:createdApiKey = $resp.key
    if (-not $script:createdApiKey) { throw "No key in response" }
    return @{ status = $r.StatusCode; key_prefix = $script:createdApiKey.Substring(0, [Math]::Min(12, $script:createdApiKey.Length)) + "..." }
}

# === 8. 列出 API Keys ===
Invoke-Test "GET /api/admin/keys (已认证)" {
    $r = Invoke-WebRequest -Uri "$BaseUrl/api/admin/keys" -TimeoutSec 10 -UseBasicParsing -WebSession $adminToken
    if ($r.StatusCode -ne 200) { throw "HTTP $($r.StatusCode)" }
    return @{ status = $r.StatusCode; keys_count = ($r.Content | ConvertFrom-Json).keys.Count }
}

# === 9. 获取账号列表 ===
Invoke-Test "GET /api/accounts (账号列表)" {
    $r = Invoke-WebRequest -Uri "$BaseUrl/api/accounts" -TimeoutSec 10 -UseBasicParsing -WebSession $adminToken
    if ($r.StatusCode -ne 200) { throw "HTTP $($r.StatusCode)" }
    return @{ status = $r.StatusCode; accounts_count = ($r.Content | ConvertFrom-Json).accounts.Count }
}

# === 10. 模型列表（需API Key）===
Invoke-Test "GET /v1/models (需API Key)" {
    if (-not $createdApiKey) { throw "No API key available" }
    $headers = @{ "Authorization" = "Bearer $createdApiKey" }
    $r = Invoke-WebRequest -Uri "$BaseUrl/v1/models" -Headers $headers -TimeoutSec 10 -UseBasicParsing
    if ($r.StatusCode -ne 200) { throw "HTTP $($r.StatusCode)" }
    $resp = $r.Content | ConvertFrom-Json
    return @{ status = $r.StatusCode; models_count = $resp.data.Count }
}

# === 11. 对话测试（需API Key）===
Invoke-Test "POST /v1/chat/completions (有API Key，无账号)" {
    if (-not $createdApiKey) { throw "No API key available" }
    $headers = @{ "Authorization" = "Bearer $createdApiKey"; "Content-Type" = "application/json" }
    $body = @{ model = "gpt-5.5"; messages = @(@{role="user";content="Say hello in one word"}) } | ConvertTo-Json
    try {
        $r = Invoke-WebRequest -Uri "$BaseUrl/v1/chat/completions" -Method POST -Headers $headers -Body $body -TimeoutSec 30 -UseBasicParsing
        return @{ status = $r.StatusCode; body = $r.Content.Substring(0, [Math]::Min(200, $r.Content.Length)) }
    } catch {
        $statusCode = $_.Exception.Response.StatusCode.value__
        $streamReader = [System.IO.StreamReader]::new($_.Exception.Response.GetResponseStream())
        $errBody = $streamReader.ReadToEnd()
        return @{ status = $statusCode; error = $errBody.Substring(0, [Math]::Min(200, $errBody.Length)) }
    }
}

# === 总结 ===
Write-Host "`n=== 测试结果 ===" -ForegroundColor Yellow
$passed = ($results | Where-Object { $_.status -eq "PASS" }).Count
$failed = ($results | Where-Object { $_.status -eq "FAIL" }).Count
$total = $results.Count
Write-Host "总计: $total | 通过: $passed | 失败: $failed" -ForegroundColor $(if ($failed -eq 0) { "Green" } else { "Red" })
Write-Host "日志: $LogFile" -ForegroundColor Gray

if ($createdApiKey) {
    Write-Host "`n[提示] 创建的测试 API Key: $createdApiKey" -ForegroundColor Yellow
    Write-Host "可执行以下命令测试对话：" -ForegroundColor Gray
    Write-Host "  curl $BaseUrl/v1/chat/completions -H `"Authorization: Bearer $createdApiKey`" -H `"Content-Type: application/json`" -d `"{\`"model\`":\`"gpt-5.5\`",\`"messages\`":[{\`"role\`":\`"user\`",\`"content\`":\`"hello\`"}]}`"" -ForegroundColor White
}
