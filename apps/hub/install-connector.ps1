# SinthMux device connector installer for Windows 10 1809+ / Windows 11.
# Run from PowerShell (the SinthMux web page generates this line):
#   & ([scriptblock]::Create((irm 'https://hub.example.com/install/connector.ps1'))) -Hub 'https://hub.example.com' -Code 'smp_...'
# tmux comes from, in order: an installed psmux, the Hub's bundled psmux, winget.
param(
  [Parameter(Mandatory = $true)][string]$Hub,
  [string]$Code = '',
  [switch]$Repair
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [Text.Encoding]::UTF8 } catch {}
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Fail([string]$Message) { Write-Host $Message -ForegroundColor Red; exit 1 }

if (-not $Code -and -not $Repair) { Fail '用法：-Hub <HTTPS 地址> -Code <配对码>；已配对设备可用 -Hub <地址> -Repair 更新' }
$Hub = $Hub.TrimEnd('/')
if ($Hub -notmatch '^https://' -and $Hub -notmatch '^http://(127\.0\.0\.1|localhost)(:\d+)?$') { Fail 'Hub 必须使用 HTTPS；HTTP 仅支持本机。' }
if ([Environment]::OSVersion.Version.Build -lt 17763) { Fail '需要 Windows 10 1809 或更新版本（终端依赖 ConPTY）。' }

$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($cpu) {
  'AMD64' { $arch = 'amd64' }
  'ARM64' { $arch = 'arm64' }
  default { Fail "不支持此 CPU 架构：$cpu" }
}

$dir = Join-Path $env:LOCALAPPDATA 'SinthMux'
New-Item -ItemType Directory -Force -Path $dir | Out-Null

function Get-HubFile([string]$Path, [string]$OutFile) {
  Invoke-WebRequest -UseBasicParsing -Uri "$Hub$Path" -OutFile $OutFile
}

function Test-Tmux([string]$Path) {
  if (-not $Path -or -not (Test-Path -LiteralPath $Path)) { return $false }
  try { & $Path -V *> $null; return $LASTEXITCODE -eq 0 } catch { return $false }
}

# Only psmux works natively: an MSYS2/Cygwin tmux cannot run outside its own environment.
function Find-Psmux {
  foreach ($name in 'psmux', 'tmux') {
    foreach ($command in @(Get-Command $name -CommandType Application -ErrorAction SilentlyContinue)) {
      $folder = Split-Path -Parent $command.Source
      if ((Test-Path -LiteralPath (Join-Path $folder 'psmux.exe')) -and (Test-Tmux $command.Source)) { return $command.Source }
    }
  }
  return $null
}

function Install-BundledTmux {
  $name = "tmux-windows-$arch.exe"
  $target = Join-Path $dir 'tmux.exe'
  $tmp = Join-Path $dir ".tmux-$PID.exe"
  $sums = Join-Path $dir ".SHA256SUMS-$PID"
  try {
    Get-HubFile "/downloads/$name" $tmp
    Get-HubFile '/downloads/SHA256SUMS' $sums
  } catch {
    Remove-Item -Force -ErrorAction SilentlyContinue $tmp, $sums
    return $null
  }
  $expected = (Get-Content $sums | Where-Object { $_ -match "^([0-9a-f]{64})\s+\*?$([regex]::Escape($name))$" } | ForEach-Object { $Matches[1] }) | Select-Object -First 1
  Remove-Item -Force $sums
  if (-not $expected -or (Get-FileHash -Algorithm SHA256 $tmp).Hash.ToLower() -ne $expected) {
    Remove-Item -Force $tmp
    Write-Host 'Hub 提供的 tmux 校验失败，改用 winget 安装。' -ForegroundColor Yellow
    return $null
  }
  Unblock-File -LiteralPath $tmp -ErrorAction SilentlyContinue
  Move-Item -Force $tmp $target
  if (Test-Tmux $target) { return $target }
  return $null
}

function Install-WingetTmux {
  if (-not (Get-Command winget -ErrorAction SilentlyContinue)) { return $null }
  Write-Host '正在用 winget 安装 psmux…'
  winget install --id marlocarlo.psmux -e --silent --accept-source-agreements --accept-package-agreements | Out-Host
  $env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
  return Find-Psmux
}

$tmux = Find-Psmux
if (-not $tmux -and (Test-Tmux (Join-Path $dir 'tmux.exe'))) { $tmux = Join-Path $dir 'tmux.exe' }
if (-not $tmux) {
  Write-Host '未检测到 tmux（Windows 版 psmux），正在从 Hub 获取…'
  $tmux = Install-BundledTmux
  if (-not $tmux) { $tmux = Install-WingetTmux }
  if (-not $tmux) { Fail 'tmux 安装未完成。请运行 winget install psmux 后重新运行这条接入命令。' }
  Write-Host "已就绪：$(& $tmux -V)（$tmux）"
}

$exe = Join-Path $dir 'sinthmux-connector.exe'
$tmp = Join-Path $dir ".sinthmux-connector-$PID.exe"
Get-HubFile "/downloads/sinthmux-connector-windows-$arch.exe" $tmp
Unblock-File -LiteralPath $tmp -ErrorAction SilentlyContinue

# The connector is a windowless program; capture its pairing output explicitly.
$start = New-Object System.Diagnostics.ProcessStartInfo $tmp
$pairArgs = "pair --hub `"$Hub`" --reuse-existing"
if ($Code) { $pairArgs += " --code `"$Code`"" }
$start.Arguments = $pairArgs
$start.UseShellExecute = $false
$start.CreateNoWindow = $true
$start.RedirectStandardOutput = $true
$start.RedirectStandardError = $true
$start.StandardOutputEncoding = [Text.Encoding]::UTF8
$start.StandardErrorEncoding = [Text.Encoding]::UTF8
$pairing = [Diagnostics.Process]::Start($start)
$stdout = $pairing.StandardOutput.ReadToEnd()
$stderr = $pairing.StandardError.ReadToEnd()
$pairing.WaitForExit()
if ($stdout) { Write-Host $stdout.TrimEnd() }
if ($pairing.ExitCode -ne 0) {
  Remove-Item -Force -ErrorAction SilentlyContinue $tmp
  Fail $stderr.TrimEnd()
}

Get-Process -Name 'sinthmux-connector' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $exe } | Stop-Process -Force
Start-Sleep -Milliseconds 300
Move-Item -Force $tmp $exe

[Environment]::SetEnvironmentVariable('SINTHMUX_TMUX_BIN', $tmux, 'User')
$env:SINTHMUX_TMUX_BIN = $tmux
& $tmux list-sessions *> $null
if ($LASTEXITCODE -ne 0) { & $tmux new-session -d -s sinthmux -c $HOME *> $null }

# Start at login for this user, and now.
Set-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -Name 'SinthMux Connector' -Value "`"$exe`""
Start-Process -FilePath $exe -WorkingDirectory $HOME
$log = Join-Path $env:APPDATA 'sinthmux\connector.log'
Write-Host "设备代理已启动，并会在登录 Windows 时自动运行。返回 SinthMux 网页确认设备在线；如果显示离线，请查看 $log。" -ForegroundColor Green
