# R-X-24: runs in the signed-in desktop session (interactive scheduled task). Drives a real console window with real key events.
param([string]$Target, [string]$Client, [string]$Out, [string]$Rows = '30', [string]$Cols = '100', [string]$Extra = '', [string]$Steps = 'info,vim,less,colour,flood,resize')
$ErrorActionPreference = 'Continue'
Add-Type -AssemblyName System.Windows.Forms, System.Drawing, Microsoft.VisualBasic
if (-not [IO.Path]::IsPathRooted($Out)) { $Out = 'C:\Users\Public\rx24\' + $Out }
if (-not $Extra) { $Extra = if (Test-Path C:\Users\Public\rx24e\extra.txt) { (Get-Content C:\Users\Public\rx24e\extra.txt -Raw).Trim() } else { '--no-customization' } }
$sid = [Diagnostics.Process]::GetCurrentProcess().SessionId
function CleanUp { Get-CimInstance Win32_Process | Where-Object { $_.SessionId -eq $sid -and (($_.Name -eq 'cmd.exe' -and $_.CommandLine -like '*title RX24*') -or $_.Name -eq 'WindowsTerminal.exe' -or ($_.Name -eq 'pb.exe' -and $_.CommandLine -like '* connect *')) } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue } }
CleanUp; Start-Sleep 2
New-Item -ItemType Directory -Force $Out | Out-Null
$pb = 'C:\Program Files\Paperboat\users\uba6bceb9290ebd807a30e102\bin\pb.exe'
function Shot($n) { $b = New-Object Drawing.Bitmap ([Windows.Forms.Screen]::PrimaryScreen.Bounds.Width), ([Windows.Forms.Screen]::PrimaryScreen.Bounds.Height)
  $g = [Drawing.Graphics]::FromImage($b); $g.CopyFromScreen(0, 0, 0, 0, $b.Size); $b.Save("$Out\$n.png"); $g.Dispose(); $b.Dispose() }
function Esc($s) { ($s.ToCharArray() | ForEach-Object { if ('+^%~(){}[]'.Contains([string]$_)) { '{' + $_ + '}' } else { [string]$_ } }) -join '' }
Add-Type -Namespace S -Name I -MemberDefinition '
[StructLayout(LayoutKind.Sequential)] public struct KI { public ushort vk; public ushort scan; public uint flags; public uint time; public IntPtr extra; }
[StructLayout(LayoutKind.Sequential)] public struct MI { public int dx, dy; public uint data, flags, time; public IntPtr extra; }
[StructLayout(LayoutKind.Explicit)] public struct U { [FieldOffset(0)] public MI mi; [FieldOffset(0)] public KI ki; }
[StructLayout(LayoutKind.Sequential)] public struct INPUT { public uint type; public U u; }
[DllImport("user32.dll", SetLastError=true)] public static extern uint SendInput(uint n, INPUT[] i, int size);
public static void Unicode(string s) { foreach (char c in s) { var a = new INPUT[2]; a[0].type = 1; a[0].u.ki.scan = c; a[0].u.ki.flags = 4; a[1].type = 1; a[1].u.ki.scan = c; a[1].u.ki.flags = 6; SendInput(2, a, System.Runtime.InteropServices.Marshal.SizeOf(typeof(INPUT))); System.Threading.Thread.Sleep(8); } }'
Add-Type -Namespace K -Name C -MemberDefinition '
[StructLayout(LayoutKind.Explicit)] public struct IR { [FieldOffset(0)] public ushort type; [FieldOffset(4)] public int down; [FieldOffset(8)] public ushort rep; [FieldOffset(10)] public ushort vk; [FieldOffset(12)] public ushort scan; [FieldOffset(14)] public ushort ch; [FieldOffset(16)] public uint ctrl; }
[DllImport("kernel32.dll", SetLastError=true)] public static extern bool FreeConsole();
[DllImport("kernel32.dll", SetLastError=true)] public static extern bool AttachConsole(uint pid);
[DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)] public static extern IntPtr CreateFile(string n, uint a, uint s, IntPtr sa, uint d, uint f, IntPtr t);
[DllImport("kernel32.dll", SetLastError=true, EntryPoint="WriteConsoleInputW")] public static extern bool WriteConsoleInput(IntPtr h, IR[] b, uint n, out uint w);
public static void Send(uint pid, ushort vk, ushort ch, uint ctrl, int times) {
  FreeConsole(); if (!AttachConsole(pid)) throw new Exception("AttachConsole " + System.Runtime.InteropServices.Marshal.GetLastWin32Error());
  IntPtr h = CreateFile("CONIN$", 0xC0000000, 3, IntPtr.Zero, 3, 0, IntPtr.Zero);
  var b = new IR[2]; b[0].type = 1; b[0].down = 1; b[0].rep = 1; b[0].vk = vk; b[0].ch = ch; b[0].ctrl = ctrl; b[1] = b[0]; b[1].down = 0;
  for (int i = 0; i < times; i++) { uint w; WriteConsoleInput(h, b, 2, out w); }
  FreeConsole(); }'
$script:conPid = 0
function ConPid { if (-not $script:conPid) { $script:conPid = (Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'cmd.exe' -and $_.CommandLine -like ('*title RX24' + $Client + '*') } | Sort-Object CreationDate -Descending | Select-Object -First 1).ProcessId }; $script:conPid }
function ConChar($c) { [K.C]::Send([uint32](ConPid), 0, [uint16][char]$c, 0, 1); Start-Sleep -Milliseconds 6 }
Add-Type -Namespace B -Name D -MemberDefinition '
[DllImport("kernel32.dll", SetLastError=true)] public static extern bool FreeConsole();
[DllImport("kernel32.dll", SetLastError=true)] public static extern bool AttachConsole(uint pid);
[DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)] public static extern IntPtr CreateFile(string n, uint a, uint s, IntPtr sa, uint d, uint f, IntPtr t);
[DllImport("kernel32.dll", SetLastError=true)] public static extern bool GetConsoleScreenBufferInfo(IntPtr h, out SBI i);
[DllImport("kernel32.dll", SetLastError=true, CharSet=CharSet.Unicode)] public static extern bool ReadConsoleOutputCharacter(IntPtr h, System.Text.StringBuilder s, uint n, uint xy, out uint r);
[StructLayout(LayoutKind.Sequential)] public struct SBI { public short sx, sy, cx, cy; public ushort attr; public short wl, wt, wr, wb, mx, my; }
[DllImport("kernel32.dll")] public static extern uint GetConsoleOutputCP(); [DllImport("kernel32.dll")] public static extern uint GetConsoleCP();
public static string Dump(uint pid) {
  FreeConsole(); if (!AttachConsole(pid)) return "AttachConsole failed";
  IntPtr h = CreateFile("CONOUT$", 0xC0000000, 3, IntPtr.Zero, 3, 0, IntPtr.Zero); SBI i; GetConsoleScreenBufferInfo(h, out i);
  var sb = new System.Text.StringBuilder(); sb.AppendLine("outputCP " + GetConsoleOutputCP() + " inputCP " + GetConsoleCP()); sb.AppendLine("window " + (i.wr - i.wl + 1) + "x" + (i.wb - i.wt + 1) + " buffer " + i.sx + "x" + i.sy);
  for (short y = i.wt; y <= i.wb; y++) { var l = new System.Text.StringBuilder(i.sx + 1); uint r; ReadConsoleOutputCharacter(h, l, (uint)i.sx, (uint)((ushort)y << 16), out r); sb.AppendLine(l.ToString().TrimEnd()); }
  FreeConsole(); return sb.ToString(); }'
function Dump($n) { Shot $n; Add-Content "$Out\conpids.txt" "$n pid=$(ConPid)"; [B.D]::Dump([uint32](ConPid)) | Set-Content -Encoding UTF8 "$Out\$n.txt" }
function Say($s) { if ($true) { foreach ($c in $s.ToCharArray()) { ConChar $c } } else { [S.I]::Unicode($s) } }
function Line($s) { Say $s; Key '{ENTER}' }
function Key($k) {
  
  $map = @{ '{ENTER}' = @(13, 13, 0); '{ESC}' = @(27, 27, 0); '{PGDN}' = @(34, 0, 0); '{HOME}' = @(36, 0, 0); '{END}' = @(35, 0, 0); '{DEL}' = @(46, 0, 0); '^c' = @(67, 3, 8); '^d' = @(68, 4, 8) }
  foreach ($t in [regex]::Matches($k, '\{[A-Z]+\}|\^[cd]')) { $m = $map[$t.Value]; [K.C]::Send([uint32](ConPid), [uint16]$m[0], [uint16]$m[1], [uint32]$m[2], 1) }
}
$title = 'RX24' + $Client
Add-Type -Namespace W -Name U -MemberDefinition '[DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h); [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow(); [DllImport("user32.dll")] public static extern void keybd_event(byte k, byte s, uint f, UIntPtr e); [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int c); [StructLayout(LayoutKind.Sequential)] public struct R { public int l, t, r, b; } [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, ref R r); [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y); [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint p); [DllImport("kernel32.dll")] public static extern uint GetCurrentThreadId(); [DllImport("user32.dll")] public static extern bool AttachThreadInput(uint a, uint b, bool f); [DllImport("user32.dll")] public static extern bool BringWindowToTop(IntPtr h); [DllImport("user32.dll")] public static extern void mouse_event(uint f, uint x, uint y, uint d, UIntPtr e); [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern IntPtr FindWindow(string c, string t);'
Add-Type -Namespace E -Name U -MemberDefinition '
public delegate bool EnumProc(IntPtr h, IntPtr l);
[DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc p, IntPtr l);
[DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
[DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr h, System.Text.StringBuilder s, int n);'
function FindByTitle($prefix) { $script:found = [IntPtr]::Zero
  $null = [E.U]::EnumWindows({ param($h, $l) if ([E.U]::IsWindowVisible($h)) { $t = New-Object Text.StringBuilder 300; [E.U]::GetWindowText($h, $t, 300) | Out-Null; if ($t.ToString().StartsWith($prefix)) { $script:found = $h; return $false } }; $true }, [IntPtr]::Zero); $script:found }
function FindWin { if ($Client -eq 'wt') { $w = Get-Process WindowsTerminal -EA SilentlyContinue | Where-Object { $_.MainWindowHandle -ne 0 } | Select-Object -First 1; if ($w) { return [IntPtr]$w.MainWindowHandle } }; $p = [uint32](ConPid); $q = Get-Process -Id $p -EA SilentlyContinue; if ($q -and $q.MainWindowHandle -ne 0) { return [IntPtr]$q.MainWindowHandle }; FindByTitle $title }
function MinimizeStrays { $h = FindByTitle 'C:\WINDOWS\SYSTEM32\cmd.exe'; if ($h -ne [IntPtr]::Zero) { [W.U]::ShowWindow($h, 6) | Out-Null } }
function Focus {
  MinimizeStrays
  $hwnd = FindWin
  if (-not $hwnd -or $hwnd -eq [IntPtr]::Zero) { Shot 'focus-no-window'; throw 'window not found' }
  $p = [pscustomobject]@{ MainWindowHandle = [IntPtr]$hwnd }
  for ($i = 0; $i -lt 5; $i++) { [W.U]::ShowWindow($p.MainWindowHandle, 9) | Out-Null; [W.U]::keybd_event(0x12, 0, 0, [UIntPtr]::Zero); [W.U]::SetForegroundWindow($p.MainWindowHandle) | Out-Null; [W.U]::keybd_event(0x12, 0, 2, [UIntPtr]::Zero); Start-Sleep -Milliseconds 400
    if ([W.U]::GetForegroundWindow() -eq $p.MainWindowHandle) { return } }
  # attach to the foreground window's input thread, the reliable way to take the foreground from a window that holds it
  $fgw = [W.U]::GetForegroundWindow(); $pp = [uint32]0; $ft = [W.U]::GetWindowThreadProcessId($fgw, [ref]$pp); $ct = [W.U]::GetCurrentThreadId()
  [W.U]::AttachThreadInput($ct, $ft, $true) | Out-Null; [W.U]::BringWindowToTop($p.MainWindowHandle) | Out-Null; [W.U]::SetForegroundWindow($p.MainWindowHandle) | Out-Null; [W.U]::AttachThreadInput($ct, $ft, $false) | Out-Null; Start-Sleep -Milliseconds 400
  if ([W.U]::GetForegroundWindow() -eq $p.MainWindowHandle) { return }
  # fall back to a real click on the title bar
  $r = New-Object W.U+R; [W.U]::GetWindowRect($p.MainWindowHandle, [ref]$r) | Out-Null
  [W.U]::SetCursorPos($r.l + 120, $r.t + 12) | Out-Null; Start-Sleep -Milliseconds 300
  [W.U]::mouse_event(2, 0, 0, 0, [UIntPtr]::Zero); Start-Sleep -Milliseconds 80; [W.U]::mouse_event(4, 0, 0, 0, [UIntPtr]::Zero); Start-Sleep -Milliseconds 600
  if ([W.U]::GetForegroundWindow() -eq $p.MainWindowHandle) { return }
  $f = [W.U]::GetForegroundWindow(); $tb = New-Object Text.StringBuilder 300; [E.U]::GetWindowText($f, $tb, 300) | Out-Null; "target=$($p.MainWindowHandle) fg=$f title=$tb" | Set-Content "$Out\focus.txt"
  Shot 'focus-uncertain'
}

Add-Type -Namespace P -Name S -MemberDefinition '[DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr h, IntPtr a, int x, int y, int w, int hh, uint f);'
function Resize($w, $h) { $hw = FindWin; [P.S]::SetWindowPos($hw, [IntPtr]::Zero, 20, 20, $w, $h, 0x14) | Out-Null; Start-Sleep 3 }
function Rect { $hw = FindWin; $r = New-Object W.U+R; [W.U]::GetWindowRect($hw, [ref]$r) | Out-Null; "$($r.r-$r.l)x$($r.b-$r.t)" }
function Launch($ConnectArgs) {
  $script:conPid = 0
  $cmdline = "title $title & " + $(if ($Client -eq 'wt') { '' } else { "mode con: cols=$Cols lines=$Rows & " }) + "`"$pb`" connect $ConnectArgs 2>>`"$Out\pb-err.txt`" & echo PB_EXIT=%errorlevel% >> `"$Out\exit.txt`""
  Add-Content "$Out\cmdline.txt" $cmdline
  if ($Client -eq 'wt') { Start-Process wt.exe -ArgumentList @('-w', 'new', '--size', "$Cols,$Rows", 'new-tab', '--suppressApplicationTitle', '--title', $title, 'cmd.exe', '/k', $cmdline) }
  else { Start-Process conhost.exe -ArgumentList @('cmd.exe', '/k', $cmdline) }
  Start-Sleep 12
  Focus
  $hw = FindWin; [P.S]::SetWindowPos($hw, [IntPtr](-1), 0, 0, 0, 0, 0x3) | Out-Null
}
function Wait-Attached { for ($k = 0; $k -lt 30; $k++) { $d = [B.D]::Dump([uint32](ConPid)); if ($d -match 'Connecting|Reconnecting') { Start-Sleep 1 } else { return } } }


$ver = (& $pb version 2>&1 | Out-String); Set-Content "$Out\pb-version.txt" $ver
$named = Test-Path C:\Users\Public\rx24e\named.txt
$sname = "rx24ew$Client" + (Get-Date -Format HHmmss)
if ($named) { Launch "$Target new --name $sname $Extra" } else { Launch "$Target $Extra" }
Wait-Attached
Line 'clear; echo TERM=$TERM LANG=$LANG; echo REC_TEST_PROMPT'; Start-Sleep 3; Dump '1-attached'
$rec = if ($named) { "terminal_00000000-0000-0000-0000-00000000000x" } else { & $pb recordings start 2>&1 | Out-String }; Set-Content "$Out\rec-start.txt" $rec
$sid = ([regex]::Match($rec, '(terminal_)?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}')).Value
if (-not $sid) { $sid = ([regex]::Match($rec, 'terminal_[A-Za-z0-9_-]+')).Value }
if ($named) { $sid = $sname }; Set-Content "$Out\sid.txt" $sid
Start-Sleep 4; Dump '2-rec'
Line 'clear; vim /tmp/rx24e.txt'; Start-Sleep 3; Say 'ihello rec vim '; Start-Sleep 2; Dump '3-vim-insert'
$r0 = Rect; Set-Content "$Out\rect.txt" $r0
Resize 640 360; Dump '4-vim-smaller'
Say 'more typing'; Start-Sleep 1
Resize 1000 640; Dump '5-vim-larger'
Say ' end'; Start-Sleep 1
Key '{ESC}'; Line ':q!'; Start-Sleep 3; Dump '6-after-vim'
Line 'echo AFTER_VIM_PROMPT_OK'; Start-Sleep 2; Dump '7-prompt'
# close the client (not the target session), attach again by id
Add-Type -Namespace M -Name P -MemberDefinition '[DllImport("user32.dll")] public static extern bool PostMessage(IntPtr h, uint m, IntPtr w, IntPtr l);'
if ($env:RX24E_CLOSE -eq 'kill' -or (Test-Path C:\Users\Public\rx24e\kill.txt)) { Get-CimInstance Win32_Process | Where-Object { $_.Name -eq 'pb.exe' -and $_.CommandLine -like '* connect *' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue } } else { $hw = FindWin; [M.P]::PostMessage($hw, 0x10, [IntPtr]::Zero, [IntPtr]::Zero) | Out-Null; Set-Content "$Out\closed-by.txt" 'WM_CLOSE' }
Start-Sleep 3
CleanUp; Start-Sleep 2
if ($sid) { Launch "$Target --session $sid $Extra"; Wait-Attached; Start-Sleep 3; Dump '8-reattached'; Line 'echo REATTACH_OK'; Start-Sleep 2; Dump '9-reattached-prompt' }
Line 'exit'; Start-Sleep 2
Line 'exit'; Start-Sleep 1
CleanUp
Set-Content "$Out\done.txt" 'done'
