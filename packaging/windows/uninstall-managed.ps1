$ErrorActionPreference = "Stop"
if (Get-Service -Name AxonPulse -ErrorAction SilentlyContinue) {
    Stop-Service AxonPulse -Force
    & sc.exe delete AxonPulse | Out-Null
}
Remove-Item -Recurse -Force -ErrorAction SilentlyContinue "$env:ProgramFiles\Axon Pulse"
Remove-Item -Recurse -Force -ErrorAction SilentlyContinue "$env:ProgramData\Axon Pulse"
Write-Host "Managed Axon Pulse and its local credentials/history were removed."
