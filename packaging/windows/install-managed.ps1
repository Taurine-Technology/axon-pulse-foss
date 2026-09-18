param(
    [string]$Binary = "$PSScriptRoot\axon-pulse.exe",
    [string]$InstallDir = "$env:ProgramFiles\Axon Pulse"
)
$ErrorActionPreference = "Stop"
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Managed installation requires an elevated PowerShell session."
}
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$stateDir = "$env:ProgramData\Axon Pulse"
New-Item -ItemType Directory -Force -Path $stateDir | Out-Null
$systemAccount = (New-Object -TypeName Security.Principal.SecurityIdentifier -ArgumentList "S-1-5-18").Translate([Security.Principal.NTAccount])
$administrators = (New-Object -TypeName Security.Principal.SecurityIdentifier -ArgumentList "S-1-5-32-544").Translate([Security.Principal.NTAccount])
$acl = Get-Acl -LiteralPath $stateDir
$acl.SetAccessRuleProtection($true, $false)
foreach ($rule in @($acl.Access)) {
    [void]$acl.RemoveAccessRuleSpecific($rule)
}
$inheritance = [Security.AccessControl.InheritanceFlags]"ContainerInherit, ObjectInherit"
$propagation = [Security.AccessControl.PropagationFlags]::None
$allow = [Security.AccessControl.AccessControlType]::Allow
foreach ($account in @($systemAccount, $administrators)) {
    $ruleArguments = $account, "FullControl", $inheritance, $propagation, $allow
    $rule = New-Object -TypeName Security.AccessControl.FileSystemAccessRule -ArgumentList $ruleArguments
    [void]$acl.AddAccessRule($rule)
}
$acl.SetOwner($administrators)
Set-Acl -LiteralPath $stateDir -AclObject $acl
Copy-Item -Force $Binary "$InstallDir\axon-pulse.exe"
if (Get-Service -Name AxonPulse -ErrorAction SilentlyContinue) {
    Stop-Service AxonPulse -Force
    & sc.exe delete AxonPulse | Out-Null
}
$command = "`"$InstallDir\axon-pulse.exe`" run --windows-service --state-dir `"$stateDir`""
New-Service -Name AxonPulse -BinaryPathName $command -DisplayName "Axon Pulse" -StartupType Automatic | Out-Null
& sc.exe description AxonPulse "Subscriber-side network quality sensor" | Out-Null
& sc.exe failure AxonPulse reset= 86400 actions= restart/5000/restart/15000/restart/60000 | Out-Null
Start-Service AxonPulse
Write-Host "Managed Axon Pulse is installed and runs before login."
