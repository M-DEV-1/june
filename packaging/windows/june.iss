; June's Windows installer: a per-user Inno Setup 7 script that needs no admin rights at any point.
; Built by packaging\release-windows.ps1 (and the release workflow), which passes the version and the folders to take files from:
;   ISCC /DAppVersion=0.2.0 /DNumericVersion=0.2.0.0 /DExeDir=<june.exe, junew.exe, june-window.exe> /DPrereqDir=<crt\, MicrosoftEdgeWebview2Setup.exe> june.iss
; Not Tauri's NSIS bundler: that one installs the window as the main program, puts a per-user install in %LOCALAPPDATA%\<productName>, which is June's data folder, and only knows how to stop and shortcut the window, while June's main program is the daemon.
; One page: the privacy notice, with the installer's two choices under it and an Install button; June opens when it is done. The notice has to be shown during installation, and the update check has to be something the user can turn off there, for the SignPath Foundation to sign June.
; Silent use, as the updater and CI run it: /VERYSILENT /SUPPRESSMSGBOXES /NORESTART, plus /RELAUNCH to open June again afterwards, even when the install fails (but never from a Setup run as administrator), and /MERGETASKS=!autostart or !updatecheck for the two choices. Uninstalling silently keeps June's data unless /PURGE is passed.
; A signed release also passes /DSignedUninstallerDir=<folder>; see [Setup].

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif
#ifndef NumericVersion
  #define NumericVersion "0.0.0.0"
#endif
#ifndef ExeDir
  #define ExeDir "..\..\dist\windows\exes"
#endif
#ifndef PrereqDir
  #define PrereqDir "..\..\dist\windows\prereqs"
#endif

; Fixed for the life of the product: Windows finds an existing install, and the upgrade path, by this id.
#define AppGuid "5BFA5136-AE33-4D06-9DBA-FBADCD341FCB"
; Windows shows a notification only under an id a Start menu shortcut carries; June's toasts name this one (internal/proactive/toast.go), so they appear as June rather than as PowerShell.
#define AppUserModelID "M-DEV-1.June"

[Setup]
AppId={{{#AppGuid}}
AppName=June
AppVersion={#AppVersion}
AppVerName=June {#AppVersion}
AppPublisher=M-DEV-1
AppPublisherURL=https://github.com/M-DEV-1/june
AppSupportURL=https://github.com/M-DEV-1/june/issues
AppUpdatesURL=https://github.com/M-DEV-1/june/releases
VersionInfoVersion={#NumericVersion}
VersionInfoTextVersion={#AppVersion}
VersionInfoProductName=June
VersionInfoProductVersion={#NumericVersion}
VersionInfoProductTextVersion={#AppVersion}
VersionInfoCompany=M-DEV-1
VersionInfoDescription=June Setup
; Per user, never elevated: {autopf} is then %LOCALAPPDATA%\Programs, and there is no "install for all users" question to answer.
PrivilegesRequired=lowest
DefaultDirName={autopf}\June
DisableDirPage=yes
DisableProgramGroupPage=yes
; The welcome, tasks, ready and finished pages are all off (the tasks page in ShouldSkipPage), which leaves the privacy notice as the only page.
DisableReadyPage=yes
DisableFinishedPage=yes
SetupArchitecture=x64
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
; ultra64's 64 MB dictionary sees junew.exe right after june.exe, which it nearly duplicates, so the second copy costs almost nothing.
Compression=lzma2/ultra64
SolidCompression=yes
; dynamic follows Windows' light or dark mode, as June's own window does.
WizardStyle=modern dynamic
InfoBeforeFile=privacy.txt
SetupIconFile=..\..\app\src-tauri\icons\icon.ico
UninstallDisplayIcon={app}\june-window.exe
UninstallDisplayName=June
OutputBaseFilename=June-Setup-x64
; Two installers at once (a double click during an update) would stop each other's June halfway.
SetupMutex=M-DEV-1.June.Setup
; June relaunches itself (see [Run]); Restart Manager stays as the net for a process the [Code] below could not stop.
CloseApplications=yes
RestartApplications=no
; The boxes show what is true now, read from the Run key and June's config when the wizard opens, not what was ticked at the last install: June's own settings can have changed either since.
UsePreviousTasks=no
#ifdef SignedUninstallerDir
; Signing June-Setup-x64.exe afterwards covers only the outer file. Setup runs a copy of its own program from %TEMP% (is-*.tmp\June-Setup-x64.tmp) and leaves another behind as unins000.exe, and both stay unsigned unless this program is signed before it is packed: Smart App Control and some of Defender's attack surface reduction rules block that unsigned copy even when the installer is signed, and Settings would show the uninstaller with no publisher.
; Inno signs it in two compiles: the first writes the unsigned program to uninst-<inno version>-<hash>.e64 in this folder and stops; once a signed copy of that file is in its place, the second takes the signature from it. packaging\release-windows.ps1 runs both, and the release workflow has the program signed with June's own between them.
SignedUninstaller=yes
SignedUninstallerDir={#SignedUninstallerDir}
#endif

[Languages]
Name: "en"; MessagesFile: "compiler:Default.isl"

[Messages]
; The privacy notice is the only page, so its heading says what the page is for rather than Inno's "Information", and its button installs (see CurPageChanged).
WizardInfoBefore=Install June
InfoBeforeLabel=What June keeps on this computer, and what it sends where.
InfoBeforeClickLabel=Read this, choose below, then click Install.

[Tasks]
; Never shown as a page: the boxes under the privacy notice stand in for them (see InitializeWizard). They stay so a silent install can still be given /MERGETASKS.
Name: "autostart"; Description: "Start June when I sign in"
Name: "updatecheck"; Description: "Check for updates"

[Files]
Source: "{#ExeDir}\june.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#ExeDir}\junew.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#ExeDir}\june-window.exe"; DestDir: "{app}"; Flags: ignoreversion
; The C++ and OpenMP runtimes the downloadable local models import; June copies them next to the models when it installs one (see internal/components).
Source: "{#PrereqDir}\crt\msvcp140.dll"; DestDir: "{app}\crt"; Flags: ignoreversion
Source: "{#PrereqDir}\crt\vcruntime140.dll"; DestDir: "{app}\crt"; Flags: ignoreversion
Source: "{#PrereqDir}\crt\vcruntime140_1.dll"; DestDir: "{app}\crt"; Flags: ignoreversion
Source: "{#PrereqDir}\crt\vcomp140.dll"; DestDir: "{app}\crt"; Flags: ignoreversion
Source: "{#PrereqDir}\MicrosoftEdgeWebview2Setup.exe"; DestDir: "{tmp}"; Flags: deleteafterinstall; Check: NeedsWebView2

[Icons]
; junew.exe with no arguments starts June's daemon if it is not running and shows the window, without a console.
Name: "{autoprograms}\June"; Filename: "{app}\junew.exe"; WorkingDir: "{app}"; IconFilename: "{app}\june-window.exe"; AppUserModelID: "{#AppUserModelID}"

[Run]
; Run without admin, Microsoft's bootstrapper installs the runtime for this user only.
Filename: "{tmp}\MicrosoftEdgeWebview2Setup.exe"; Parameters: "/silent /install"; StatusMsg: "Installing the Microsoft Edge WebView2 Runtime..."; Flags: waituntilterminated; Check: NeedsWebView2; AfterInstall: ReportWebView2
; june.exe --autostart writes the choice into June's config and the HKCU Run key in one step, the same as the switch in June's settings; --update-check writes its choice into June's config the same way.
Filename: "{app}\june.exe"; Parameters: "--autostart on"; WorkingDir: "{app}"; StatusMsg: "Setting June to start when you sign in..."; Flags: runhidden waituntilterminated; Check: ShouldTurnAutostartOn
Filename: "{app}\june.exe"; Parameters: "--autostart off"; WorkingDir: "{app}"; Flags: runhidden waituntilterminated; Check: ShouldTurnAutostartOff
Filename: "{app}\june.exe"; Parameters: "--update-check off"; WorkingDir: "{app}"; Flags: runhidden waituntilterminated; Check: ShouldTurnUpdateCheckOff
Filename: "{app}\june.exe"; Parameters: "--update-check on"; WorkingDir: "{app}"; Flags: runhidden waituntilterminated; Check: ShouldTurnUpdateCheckOn
Filename: "{app}\junew.exe"; WorkingDir: "{app}"; Flags: nowait; Check: RelaunchAfterSilentInstall; AfterInstall: MarkRelaunched
; There is no finished page to offer "Open June" on: June opens, and its own setup takes over from here.
Filename: "{app}\junew.exe"; WorkingDir: "{app}"; Flags: nowait skipifsilent runasoriginaluser; Check: OpenAfterInstall

[Code]
const
  RunKey = 'Software\Microsoft\Windows\CurrentVersion\Run';
  RunValue = 'June';
  UninstallKey = 'Software\Microsoft\Windows\CurrentVersion\Uninstall\{{#AppGuid}}_is1';
  WebView2Client = 'Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}';
  WebView2Url = 'https://go.microsoft.com/fwlink/p/?LinkId=2124703';
  { What June writes at the top of its data folder (config.DataDir and what the daemon hands it to): files, which may be patterns, then folders, each list separated by '|'. }
  JuneDataFiles = 'db|db-wal|db-shm|db-journal|june-config.json*|ipc-token*|env|env.tmp-*|june.log*|window.log|crash.log|brain_quota.json*|brain_usage.json*|codex-install-id|open-window|paused|dream-now|portal-input-token|june-config.set-aside';
  JuneDataFolders = 'components|recordings|frames|vectors|models|whispercpp|sherpa|llama.cpp|update|dreams|study|replays|voice-previews';

var
  Upgrading: Boolean;
  { Whether June's config has the daily update check turned off, from June's settings or an earlier install. }
  UpdateCheckWasOff: Boolean;
  { The installer's two choices, under the privacy notice. }
  AutostartBox: TNewCheckBox;
  UpdateCheckBox: TNewCheckBox;
  { From the moment PrepareToInstall stops June until [Run] opens it again; see DeinitializeSetup. }
  StoppedJune: Boolean;
  { Whether a June from this install folder was running when Setup stopped it, so a Setup that then fails puts back only a June the user had. }
  WasRunning: Boolean;
  InstallFinished: Boolean;
  Relaunched: Boolean;

function HasParam(const Name: String): Boolean;
var
  I: Integer;
begin
  Result := False;
  for I := 1 to ParamCount do
    if CompareText(ParamStr(I), Name) = 0 then
    begin
      Result := True;
      Exit;
    end;
end;

{ Where June keeps its data: JUNE_DATA_DIR when set, as config.DataDir resolves it, else %LOCALAPPDATA%\june. }
function JuneDataDir(): String;
begin
  Result := GetEnv('JUNE_DATA_DIR');
  if Result = '' then
    Result := ExpandConstant('{localappdata}\june');
  Result := RemoveBackslashUnlessRoot(Result);
end;

{ Go writes the field as "update_check": false only when it has been turned off; a config without it checks. }
function ConfigUpdateCheckOff(): Boolean;
var
  Raw: AnsiString;
  Text: String;
begin
  Result := False;
  if not LoadStringFromFile(JuneDataDir() + '\june-config.json', Raw) then
    Exit;
  Text := String(Raw);
  StringChangeEx(Text, ' ', '', True);
  StringChangeEx(Text, #9, '', True);
  Result := Pos('"update_check":false', Text) > 0;
end;

function InitializeSetup(): Boolean;
begin
  Upgrading := RegKeyExists(HKA, UninstallKey);
  UpdateCheckWasOff := ConfigUpdateCheckOff();
  Result := True;
end;

{ Two boxes under the privacy notice rather than Inno's tasks page, which would be a second page for two choices. Start at sign-in starts as things stand only on an upgrade, from the Run key; a first install ticks it even over data an earlier install kept. Check for updates starts from June's config whenever there is one. }
procedure InitializeWizard();
var
  Memo: TRichEditViewer;
  BoxHeight, Gap: Integer;
begin
  Memo := WizardForm.InfoBeforeMemo;
  BoxHeight := ScaleY(17);
  Gap := ScaleY(8);
  Memo.Height := Memo.Height - Gap - BoxHeight - ScaleY(4) - BoxHeight;

  AutostartBox := TNewCheckBox.Create(WizardForm);
  AutostartBox.Parent := WizardForm.InfoBeforePage;
  AutostartBox.Caption := 'Start June when I sign in';
  AutostartBox.Left := Memo.Left;
  AutostartBox.Top := Memo.Top + Memo.Height + Gap;
  AutostartBox.Width := Memo.Width;
  AutostartBox.Height := BoxHeight;
  AutostartBox.Anchors := [akLeft, akRight, akBottom];
  AutostartBox.Checked := not Upgrading or RegValueExists(HKCU, RunKey, RunValue);

  UpdateCheckBox := TNewCheckBox.Create(WizardForm);
  UpdateCheckBox.Parent := WizardForm.InfoBeforePage;
  UpdateCheckBox.Caption := 'Check for updates';
  UpdateCheckBox.Left := Memo.Left;
  UpdateCheckBox.Top := AutostartBox.Top + BoxHeight + ScaleY(4);
  UpdateCheckBox.Width := Memo.Width;
  UpdateCheckBox.Height := BoxHeight;
  UpdateCheckBox.Anchors := [akLeft, akRight, akBottom];
  UpdateCheckBox.Checked := not UpdateCheckWasOff;
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := PageID = wpSelectTasks;
end;

{ Inno names the button Install only on the Ready page, which is turned off; here, clicking it installs. }
procedure CurPageChanged(CurPageID: Integer);
begin
  if CurPageID = wpInfoBefore then
    WizardForm.NextButton.Caption := SetupMessage(msgButtonInstall);
end;

{ Space-separated "/PID n" arguments for taskkill, one per June process whose program lives in Dir; '?' when Windows cannot be asked. }
function JuneProcesses(const Dir: String): String;
var
  Locator, Service, Procs, Proc, Path: Variant;
  Prefix: String;
  I: Integer;
begin
  Result := '';
  Prefix := Lowercase(AddBackslash(Dir));
  try
    Locator := CreateOleObject('WbemScripting.SWbemLocator');
    Service := Locator.ConnectServer('.', 'root\CIMV2');
    Procs := Service.ExecQuery('SELECT ProcessId, ExecutablePath FROM Win32_Process WHERE Name = ''june.exe'' OR Name = ''junew.exe'' OR Name = ''june-window.exe''');
    for I := 0 to Procs.Count - 1 do
    begin
      Proc := Procs.ItemIndex(I);
      Path := Proc.ExecutablePath;
      { Another user's processes come back without a path; they are not this install's to stop. }
      if VarIsNull(Path) or VarIsEmpty(Path) then
        Continue;
      if Pos(Prefix, Lowercase(Path)) = 1 then
        Result := Result + ' /PID ' + IntToStr(Proc.ProcessId);
    end;
  except
    Result := '?';
  end;
end;

procedure TaskKill(const Args: String);
var
  Code: Integer;
begin
  Exec(ExpandConstant('{sys}\taskkill.exe'), Args, '', SW_HIDE, ewWaitUntilTerminated, Code);
  Log(Format('taskkill %s exited %d', [Args, Code]));
end;

{ Stops June before the files in Dir are replaced or removed. Quitter is a june.exe whose --quit asks the June daemon answering on June's port to shut down the way the tray's Quit does, wherever that June was started from; the window and every model server it started end with it (they share its job object).
  A daemon too old or too stuck to answer is then closed like a window would be, given five seconds, and killed. That fallback goes by folder, as the programs in Dir are the ones that would block the files and a June elsewhere that would not quit is not this Setup's to kill; only when Windows cannot list processes at all does it go by program name.
  Never with /T: the daemon's window and model servers die with its job object anyway, while its process tree also holds apps it opened for the user (a Start menu shortcut it started itself), which are the user's and must survive an update. }
procedure StopJune(const Quitter, Dir: String);
var
  Code, I: Integer;
  Pids: String;
begin
  if FileExists(Quitter) then
  begin
    Exec(Quitter, '--quit', ExtractFileDir(Quitter), SW_HIDE, ewWaitUntilTerminated, Code);
    Log(Format('june.exe --quit exited %d', [Code]));
  end;
  Pids := JuneProcesses(Dir);
  if Pids = '' then
    Exit;
  if Pids = '?' then
  begin
    Log('could not list processes; stopping June by program name');
    TaskKill('/IM junew.exe /IM june.exe /IM june-window.exe');
    Sleep(5000);
    TaskKill('/F /IM junew.exe /IM june.exe /IM june-window.exe');
    Exit;
  end;
  TaskKill(Trim(Pids));
  for I := 1 to 10 do
  begin
    Sleep(500);
    Pids := JuneProcesses(Dir);
    if Pids = '' then
      Exit;
  end;
  TaskKill('/F' + Pids);
  Sleep(500);
end;

{ The june.exe this Setup carries does the quitting, unpacked to Setup's temporary folder, even on an upgrade: a first install has no june.exe of its own yet, and a June already running from somewhere else (a portable copy, a development build) would keep June's port, so the June opened at the end would be that one and not this one. }
function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  WasRunning := JuneProcesses(ExpandConstant('{app}')) <> '';
  ExtractTemporaryFile('june.exe');
  StopJune(ExpandConstant('{tmp}\june.exe'), ExpandConstant('{app}'));
  StoppedJune := True;
  Result := '';
end;

function HasWebView2At(const RootKey: Integer; const SubKey: String): Boolean;
var
  Version: String;
begin
  Result := RegQueryStringValue(RootKey, SubKey, 'pv', Version) and (Version <> '') and (Version <> '0.0.0.0');
end;

{ The same places doctor_windows.go looks: Microsoft's per-machine runtime under the 32-bit view, and a per-user one under HKCU. }
function WebView2Installed(): Boolean;
begin
  Result := HasWebView2At(HKLM32, 'SOFTWARE\' + WebView2Client) or HasWebView2At(HKLM64, 'SOFTWARE\' + WebView2Client) or HasWebView2At(HKCU, 'Software\' + WebView2Client);
end;

function NeedsWebView2(): Boolean;
begin
  Result := not WebView2Installed();
end;

procedure ReportWebView2();
begin
  if not WebView2Installed() then
    SuppressibleMsgBox('June''s window needs the Microsoft Edge WebView2 Runtime, and installing it did not work. June is installed; to finish, install WebView2 from ' + WebView2Url + ' and then open June from the Start menu.', mbError, MB_OK, IDOK);
end;

{ A silent upgrade is June updating itself, and whatever the user has since chosen in June's settings stands. Anything else applies the choices as they were left. }
function ChoicesApply(): Boolean;
begin
  Result := not (Upgrading and WizardSilent());
end;

{ The boxes, or on a silent install, where no page is shown, the tasks as /MERGETASKS left them. }
function AutostartChosen(): Boolean;
begin
  if WizardSilent() then
    Result := WizardIsTaskSelected('autostart')
  else
    Result := AutostartBox.Checked;
end;

function UpdateCheckChosen(): Boolean;
begin
  if WizardSilent() then
    Result := WizardIsTaskSelected('updatecheck')
  else
    Result := UpdateCheckBox.Checked;
end;

function ShouldTurnAutostartOn(): Boolean;
begin
  Result := ChoicesApply() and AutostartChosen();
end;

function ShouldTurnAutostartOff(): Boolean;
begin
  Result := ChoicesApply() and not AutostartChosen();
end;

function ShouldTurnUpdateCheckOff(): Boolean;
begin
  Result := ChoicesApply() and not UpdateCheckChosen();
end;

{ A config that never had the check turned off already checks, so only a box ticked over one that had is written back. A silent install never turns it on: its task is ticked by default, and a default is no reason to undo the user's own choice. }
function ShouldTurnUpdateCheckOn(): Boolean;
begin
  Result := not WizardSilent() and UpdateCheckWasOff and UpdateCheckBox.Checked;
end;

{ Whether Setup runs with an administrator's full rights while Windows would give this user a normal session: started with Run as administrator, or from an elevated window, with UAC on. A June opened from here would keep those rights in everything it starts, the window, PowerShell and the AI command lines included, until the next sign-in. With UAC off every program runs that way and there is nothing to avoid; the built-in Administrator, whose every program is elevated too, is not told apart here and only has to open June from the Start menu itself. }
function SetupElevated(): Boolean;
var
  Lua: Cardinal;
begin
  Result := IsAdmin() and not (RegQueryDWordValue(HKLM, 'SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System', 'EnableLUA', Lua) and (Lua = 0));
end;

{ /RELAUNCH reaches an elevated Setup too, from winget in an administrator terminal or a script run as administrator, and runasoriginaluser cannot help there: Setup never raised itself, so the original user is the elevated one. }
function RelaunchAfterSilentInstall(): Boolean;
begin
  Result := WizardSilent() and HasParam('/RELAUNCH');
  if Result and SetupElevated() then
  begin
    Log('/RELAUNCH: June was not opened, because Setup runs as administrator and June would keep those rights; open it from the Start menu');
    Result := False;
  end;
end;

procedure MarkRelaunched();
begin
  Relaunched := True;
end;

function OpenAfterInstall(): Boolean;
begin
  Result := not SetupElevated();
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  { Every file is in place: from here on, whether June opens is [Run]'s to decide. }
  if CurStep = ssPostInstall then
    InstallFinished := True;
  if (CurStep = ssDone) and not WizardSilent() and SetupElevated() then
    MsgBox('June is installed. Setup was run as administrator, and a June opened from here would keep those rights, so it was not opened: open June from the Start menu.', mbInformation, MB_OK);
end;

{ A Setup that fails or is cancelled once June is stopped (a full disk, a file another program holds open, Cancel during the copy) ends before [Run], so nothing would open June again. After a silent update its /RELAUNCH entry never runs, and the updater that started Setup went down with that June; after the wizard, the user is left to find June in the Start menu, or to wait for the next sign-in.
  So whichever June is now on disk, the old one or a partly updated one, is opened again here, when the June Setup stopped was running or /RELAUNCH asked for it. A finished install is left to [Run]. The reason for a failure is in Setup's log, which the updater asks for with /LOG. }
procedure DeinitializeSetup();
var
  Code: Integer;
  Junew: String;
begin
  if not StoppedJune or InstallFinished or Relaunched then
    Exit;
  if not WasRunning and not RelaunchAfterSilentInstall() then
    Exit;
  if SetupElevated() then
  begin
    Log('Setup did not finish; June was not opened again, because Setup runs as administrator and June would keep those rights');
    Exit;
  end;
  Junew := ExpandConstant('{app}\junew.exe');
  if not FileExists(Junew) then
    Exit;
  Log('Setup did not finish; opening the June that is installed again');
  Exec(Junew, '', ExpandConstant('{app}'), SW_SHOWNORMAL, ewNoWait, Code);
end;

{ The Run value is removed only when it starts this copy of June: one written by a June elsewhere on the machine is that copy's to keep. }
procedure RemoveRunValue(const Dir: String);
var
  Command: String;
begin
  if RegQueryStringValue(HKCU, RunKey, RunValue, Command) and (Pos(Lowercase(AddBackslash(Dir)), Lowercase(Command)) > 0) then
    RegDeleteValue(HKCU, RunKey, RunValue);
end;

{ A folder is deleted only when it holds what June writes there, never a drive root or a folder that merely exists, so a mistyped JUNE_DATA_DIR cannot turn into deleting someone's files. }
function IsJuneDataDir(const Dir: String): Boolean;
begin
  Result := (Length(Dir) > 3) and DirExists(Dir) and
    (CompareText(Dir, ExpandConstant('{localappdata}')) <> 0) and (CompareText(Dir, GetEnv('USERPROFILE')) <> 0) and
    (FileExists(Dir + '\june-config.json') or FileExists(Dir + '\ipc-token') or FileExists(Dir + '\db'));
end;

{ Deletes each name in a '|'-separated list from Dir: files, which may be patterns, or with Folders, folders and everything in them. }
procedure DeleteEach(const Dir, Names: String; const Folders: Boolean);
var
  Rest: String;
  P: Integer;
begin
  Rest := Names;
  while Rest <> '' do
  begin
    P := Pos('|', Rest);
    if P = 0 then
      P := Length(Rest) + 1;
    DelTree(Dir + '\' + Copy(Rest, 1, P - 1), Folders, True, Folders);
    Delete(Rest, 1, P);
  end;
end;

procedure PurgeData();
var
  Dir: String;
begin
  Dir := JuneDataDir();
  if not IsJuneDataDir(Dir) then
  begin
    Log('no June data folder at ' + Dir);
    Exit;
  end;
  if UninstallSilent() then
  begin
    if not HasParam('/PURGE') then
      Exit;
  end
  else if not HasParam('/PURGE') then
  begin
    if MsgBox('Also delete your June data (memory, recordings, downloaded models)?' + #13#10#13#10 + Dir + #13#10#13#10 + 'Choose No to keep it for a later install.', mbConfirmation, MB_YESNO or MB_DEFBUTTON2) <> IDYES then
      Exit;
  end;
  if GetEnv('JUNE_DATA_DIR') = '' then
  begin
    DelTree(Dir, True, True, True);
    { The window's WebView2 profile is shared by every June of this user, so it goes only with the default data folder, not with a JUNE_DATA_DIR install. }
    DelTree(ExpandConstant('{localappdata}\dev.june.app'), True, True, True);
    if DirExists(Dir) then
    begin
      Log('could not remove all of ' + Dir);
      if not UninstallSilent() then
        MsgBox('Some of June''s data could not be removed. It is in:' + #13#10#13#10 + Dir, mbInformation, MB_OK);
    end;
    Exit;
  end;
  { JUNE_DATA_DIR can name a folder that holds more than June's data, so only what June writes there goes, and the folder itself only when that leaves it empty. }
  DeleteEach(Dir, JuneDataFiles, False);
  DeleteEach(Dir, JuneDataFolders, True);
  if not RemoveDir(Dir) then
  begin
    Log('left ' + Dir + ': it still holds files that are not June''s, or that could not be removed');
    if not UninstallSilent() then
      MsgBox('June''s data was deleted. Its folder still holds other files, which were left as they are:' + #13#10#13#10 + Dir, mbInformation, MB_OK);
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  case CurUninstallStep of
    { After the "remove June?" confirmation and before any file goes: a June still running holds its own exes open. }
    usUninstall:
      begin
        StopJune(ExpandConstant('{app}\june.exe'), ExpandConstant('{app}'));
        RemoveRunValue(ExpandConstant('{app}'));
      end;
    usPostUninstall:
      PurgeData();
  end;
end;
