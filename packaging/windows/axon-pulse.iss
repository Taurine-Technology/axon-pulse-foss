#define AppName "Axon Pulse"
#ifndef AppVersion
  #define AppVersion "0.0.1"
#endif
#ifndef SourceExe
  #define SourceExe "..\..\bin\axon-pulse-desktop.exe"
#endif
#ifndef AppArch
  #define AppArch "amd64"
#endif

[Setup]
AppId={{9FD9966E-2454-4F01-B094-EE7EDDE60612}
AppName={#AppName}
AppVersion={#AppVersion}
DefaultDirName={localappdata}\Programs\Axon Pulse
DefaultGroupName=Axon Pulse
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
OutputBaseFilename=axon-pulse_windows_{#AppArch}_setup
Compression=lzma2/ultra64
SolidCompression=yes
#if AppArch == "arm64"
ArchitecturesAllowed=arm64
ArchitecturesInstallIn64BitMode=arm64
#else
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
#endif
UninstallDisplayIcon={app}\Axon Pulse.exe
WizardStyle=modern

[Files]
Source: "{#SourceExe}"; DestDir: "{app}"; DestName: "Axon Pulse.exe"; Flags: ignoreversion

[Icons]
Name: "{group}\Axon Pulse"; Filename: "{app}\Axon Pulse.exe"
Name: "{userdesktop}\Axon Pulse"; Filename: "{app}\Axon Pulse.exe"; Tasks: desktopicon

[Tasks]
Name: desktopicon; Description: "Create a desktop shortcut"; Flags: unchecked

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "AxonPulse"; ValueData: """{app}\Axon Pulse.exe"""; Flags: uninsdeletevalue
Root: HKCU; Subkey: "Software\Classes\axon-pulse"; ValueType: string; ValueData: "URL:Axon Pulse Claim"; Flags: uninsdeletekey
Root: HKCU; Subkey: "Software\Classes\axon-pulse"; ValueType: string; ValueName: "URL Protocol"; ValueData: ""
Root: HKCU; Subkey: "Software\Classes\axon-pulse\DefaultIcon"; ValueType: string; ValueData: """{app}\Axon Pulse.exe"",0"
Root: HKCU; Subkey: "Software\Classes\axon-pulse\shell\open\command"; ValueType: string; ValueData: """{app}\Axon Pulse.exe"" ""%1"""

[Run]
Filename: "{app}\Axon Pulse.exe"; Description: "Open Axon Pulse"; Flags: nowait postinstall skipifsilent

[UninstallDelete]
Type: filesandordirs; Name: "{userappdata}\axon-pulse"
Type: filesandordirs; Name: "{localappdata}\axon-pulse"
