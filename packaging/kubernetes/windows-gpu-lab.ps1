# Native Windows specialist preparation. Run via the owner's existing Windows
# SSH path. This script does not change WSL, drivers, services, SSH or firewalls.
[CmdletBinding()]
param(
    [ValidateSet('Inventory', 'Encode', 'Start')][string]$Action = 'Inventory',
    [string]$LabRoot = 'C:\ether-gpu-lab',
    [string]$PostgresHost = '127.0.0.1',
    [int]$PostgresPort = 15435,
    [string[]]$PrivatePeers = @(),
    [string]$AdvertisedOrigin = '',
    [int]$PrivatePort = 32411,
    [int]$ProbePort = 32412,
    [int]$LedgerCapacity = 1000000,
    [ValidateRange(0,60000)][int]$ClockSkewMillis = 1000,
    [switch]$KnownClockBound
)
$ErrorActionPreference = 'Stop'
if ([IO.Path]::GetFullPath($LabRoot).TrimEnd('\') -notin @('C:\ether-gpu-lab','D:\ether-gpu-lab')) { throw 'Only an owned C:\ether-gpu-lab or D:\ether-gpu-lab directory is allowed' }
# Inventory must remain read-only, including on a host with no staged lab.
& nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv,noheader
if ($LASTEXITCODE -ne 0) { throw 'Native NVIDIA inventory failed' }
Get-NetIPAddress -AddressFamily IPv4 | Select-Object InterfaceAlias,IPAddress | ConvertTo-Json -Compress
if ($Action -eq 'Inventory') { return }
function Assert-WSLMemoryHeadroom {
    # Read the existing distro's PSI immediately before each heavy operation.
    # Never restart WSL or change its memory configuration to pass this gate.
    $Pressure = (& wsl.exe --distribution Ubuntu-24.04 --exec cat /proc/pressure/memory) -join "`n"
    if ($LASTEXITCODE -ne 0 -or $Pressure -notmatch '(?m)^full avg10=([0-9]+(?:\.[0-9]+)?)') { throw 'Cannot verify WSL memory pressure' }
    $Full = [double]::Parse($Matches[1],[Globalization.CultureInfo]::InvariantCulture)
    Write-Output $Pressure
    if ($Full -gt 10) { throw 'WSL full avg10 exceeds 10; wait and recheck before encoding or starting the specialist' }
}
New-Item -ItemType Directory -Force -Path $LabRoot | Out-Null
$Secrets = Join-Path $LabRoot 'secrets'
$Data = Join-Path $LabRoot 'config'
$Scratch = Join-Path $LabRoot 'scratch'
$Cache = Join-Path $LabRoot 'cache'
$Logs = Join-Path $LabRoot 'logs'
foreach ($Path in @($Secrets,$Data,$Scratch,$Cache,$Logs)) { New-Item -ItemType Directory -Force -Path $Path | Out-Null }
# Restrict the lab's credential directory without changing any host policy.
$User = [Security.Principal.WindowsIdentity]::GetCurrent().Name
foreach ($PrivateDirectory in @($Secrets,$Data)) {
    $Acl = Get-Acl $PrivateDirectory
    $Acl.SetAccessRuleProtection($true,$false)
    $Acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($User,'FullControl','ContainerInherit,ObjectInherit','None','Allow')))
    $Acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule('SYSTEM','FullControl','ContainerInherit,ObjectInherit','None','Allow')))
    Set-Acl $PrivateDirectory $Acl
}
$FFmpegDirectory = Join-Path $LabRoot 'ffmpeg-8.1.3-1'
$FFmpeg = Join-Path $FFmpegDirectory 'ffmpeg.exe'
$Ether = Join-Path $LabRoot 'ether.exe'
$Launcher = Join-Path $LabRoot 'ether-lab-launcher.exe'
if (-not (Test-Path $FFmpeg)) { throw 'Stage the checksum-verified pinned Windows FFmpeg bundle in the lab directory first' }
& $FFmpeg -hide_banner -version | Select-Object -First 1
# This executes an actual native NVENC encode; encoder listings alone are not proof.
Assert-WSLMemoryHeadroom
$Smoke = New-Object Diagnostics.Process
$Smoke.StartInfo = New-Object Diagnostics.ProcessStartInfo
$Smoke.StartInfo.FileName = $FFmpeg
$Smoke.StartInfo.Arguments = '-hide_banner -nostdin -v error -f lavfi -i testsrc2=size=640x360:rate=24 -frames:v 48 -c:v h264_nvenc -progress pipe:1 -f null -'
$Smoke.StartInfo.UseShellExecute = $false
$Smoke.StartInfo.CreateNoWindow = $true
$Smoke.StartInfo.RedirectStandardOutput = $true
$Smoke.StartInfo.RedirectStandardError = $true
$SmokeStarted = $false
try {
    $SmokeStarted = $Smoke.Start()
    if (-not $SmokeStarted) { throw 'Native NVENC encode did not start' }
    $SmokeHandle = $Smoke.Handle
    $Smoke.PriorityClass = 'BelowNormal'
    $SmokeOutput = $Smoke.StandardOutput.ReadToEndAsync()
    $SmokeErrors = $Smoke.StandardError.ReadToEndAsync()
    if (-not $Smoke.WaitForExit(20000)) { throw 'Native NVENC encode timed out' }
    if (-not $SmokeOutput.Wait(3000) -or -not $SmokeErrors.Wait(3000)) { throw 'Native NVENC log pipes did not close' }
    [IO.File]::WriteAllText((Join-Path $Logs 'nvenc-smoke.log'),$SmokeErrors.Result)
    if ($Smoke.ExitCode -ne 0 -or $SmokeOutput.Result -notmatch '(?m)^frame=48\r?$') { throw 'Native NVENC encode failed; inspect the owned lab log' }
}
finally {
    if ($SmokeStarted -and -not $Smoke.HasExited) { $Smoke.Kill(); [void]$Smoke.WaitForExit(10000) }
    $Smoke.Dispose()
}
if ($Action -eq 'Encode') { Write-Output 'Native generated NVENC encode passed; distributed playback not tested'; return }
if (-not $KnownClockBound) { throw 'A verified clock bound is required before specialist startup' }
if (-not (Test-Path $Ether)) { throw 'Stage exact reviewed Windows Ether candidate first' }
if (Test-Path (Join-Path $LabRoot 'specialist.pid')) { throw 'Existing owned PID record needs explicit review before another start' }
if ([string]::IsNullOrEmpty($AdvertisedOrigin) -or $PrivatePeers.Count -eq 0) { throw 'Explicit bidirectional advertised origin and private peers are required' }
$Origin = [Uri]$AdvertisedOrigin
if ($Origin.Scheme -ne 'https' -or $Origin.Port -lt 1) { throw 'Invalid private origin' }
$Reachable = Test-NetConnection -ComputerName $PostgresHost -Port $PostgresPort -InformationLevel Quiet
if (-not $Reachable) { throw 'Lab PostgreSQL transport is unreachable' }
foreach ($Peer in $PrivatePeers) {
    $Uri = [Uri]$Peer
    if ($Uri.Scheme -ne 'https' -or -not (Test-NetConnection -ComputerName $Uri.Host -Port $Uri.Port -InformationLevel Quiet)) { throw 'Private peer transport is unreachable' }
}
foreach ($Name in @('database-url','cluster-key','ca.crt','ca.key','roots.pem')) {
    if (-not (Test-Path (Join-Path $Secrets $Name))) { throw "Required lab Secret file missing: $Name" }
}
if ((Get-Item (Join-Path $Secrets 'cluster-key')).Length -ne 32) { throw 'cluster-key must be exactly 32 raw bytes' }
$Deployment = @{
    bootstrap=$false; roles=@('transcode'); internalAddress=$AdvertisedOrigin
    privateListen=":$PrivatePort"; probeListen="127.0.0.1:$ProbePort"
    capacity=1; ledgerCapacity=$LedgerCapacity; clockBoundKnown=$true; clockSkewMillis=$ClockSkewMillis
    databaseURLFile=(Join-Path $Secrets 'database-url'); clusterKeyFile=(Join-Path $Secrets 'cluster-key')
    caFile=(Join-Path $Secrets 'ca.crt'); caKeyFile=(Join-Path $Secrets 'ca.key'); rootsFile=(Join-Path $Secrets 'roots.pem')
    tempDir=$Scratch; cacheDir=$Cache; resourceScope='ether035-native-rtx4080'; drainSeconds=120
}
$DeploymentPath = Join-Path $LabRoot 'deployment.json'
[IO.File]::WriteAllText($DeploymentPath,($Deployment | ConvertTo-Json),[Text.UTF8Encoding]::new($false))
$env:ETHER_DEPLOYMENT_FILE = $DeploymentPath
$env:ETHER_DATA_DIR = $Data
$env:XDG_CACHE_HOME = $Cache
$env:PATH = $FFmpegDirectory + [IO.Path]::PathSeparator + $env:PATH
# No secrets on the command line. This is an owned foreground-service process,
# not a Windows Service. Keep its exact PID and stop/drain only that process.
if (-not (Test-Path $Launcher)) { throw 'Stage reviewed owned job launcher before Start' }
$Process = $null
try {
    Assert-WSLMemoryHeadroom
    $Process = Start-Process -FilePath $Launcher -WorkingDirectory $LabRoot -PassThru `
        -RedirectStandardOutput (Join-Path $Logs 'ether.stdout.log') -RedirectStandardError (Join-Path $Logs 'ether.stderr.log')
    $OwnedHandle = $Process.Handle # Retain the exact native object before status/exit inspection.
    $Process.PriorityClass = 'BelowNormal'
    [IO.File]::WriteAllText((Join-Path $LabRoot 'specialist.pid'),[string]$Process.Id)
}
catch {
    # This exact launcher Process object was created above; never stop by name.
    # Closing its noninheritable job handle kills Ether and all descendants.
    if ($null -ne $Process) {
        try {
            $Process.Refresh()
            if (-not $Process.HasExited) { $Process.Kill(); [void]$Process.WaitForExit(10000) }
        }
        catch {
            # Retain exact PID for owner recovery when cleanup itself fails.
            [Console]::Error.WriteLine("Owned native child cleanup failed; inspect exact PID=$($Process.Id)")
        }
    }
    throw
}
Write-Output "Native lab specialist started, owned PID=$($Process.Id); startup/media/NVENC execution still require verification"
