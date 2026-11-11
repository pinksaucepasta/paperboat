# Runs in the signed-in desktop session (windows_console_flash.py starts it through an interactive scheduled task).
# Starts -Exe DETACHED (-Inherit: with this script's console, like a command the user types in their terminal) (no console, like a service, the updater or a RunOnce entry) and watches the desktop for
# new visible console windows until -Seconds after it exits. Writes JSON to -Out. Exit 1 on a flash.
param([string]$Exe, [string]$ArgumentLine = '', [int]$Seconds = 30, [int]$Timeout = 0, [string]$Log = '', [string]$PathPrepend = '', [switch]$Inherit, [string]$Out, [switch]$Control)
Add-Type -TypeDefinition @'
using System; using System.Collections.Generic; using System.Runtime.InteropServices; using System.Text;
public static class Win {
  delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc p, IntPtr l);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  public static List<string> Visible() {
    var r = new List<string>();
    EnumWindows((h, l) => { if (IsWindowVisible(h)) { var c = new StringBuilder(256); var t = new StringBuilder(256); uint pid;
      GetClassName(h, c, 256); GetWindowText(h, t, 256); GetWindowThreadProcessId(h, out pid);
      r.Add(h.ToInt64() + "|" + pid + "|" + c + "|" + t); } return true; }, IntPtr.Zero); return r; }
  [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)] struct SI { public int cb; public string a, b, c; public int x, y, w, h, xc, yc, fill, flags; public short show, res; public IntPtr r2, i, o, e; }
  [StructLayout(LayoutKind.Sequential)] struct PI { public IntPtr hp, ht; public uint pid, tid; }
  [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)] static extern bool CreateProcessW(string app, StringBuilder cmd, IntPtr pa, IntPtr ta, bool inh, uint flags, IntPtr env, string dir, ref SI si, out PI pi);
  [DllImport("kernel32.dll")] static extern uint WaitForSingleObject(IntPtr h, uint ms);
  [DllImport("kernel32.dll")] static extern bool GetExitCodeProcess(IntPtr h, out uint code);
  // DETACHED_PROCESS: the child has no console, so any console program it starts without CREATE_NO_WINDOW opens a window.
  [StructLayout(LayoutKind.Sequential)] struct SA { public int n; public IntPtr d; public bool inh; }
  [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)] static extern IntPtr CreateFileW(string f, uint access, uint share, ref SA sa, uint disp, uint attr, IntPtr t);
  [DllImport("kernel32.dll")] static extern bool CloseHandle(IntPtr h);
  // log: the child's stdout and stderr go to this file (inherited handles; still no console); empty: none.
  public static int RunDetached(string cmd, uint timeoutMs, string log, bool inherit_console, out uint pid) {
    var si = new SI(); si.cb = System.Runtime.InteropServices.Marshal.SizeOf(si); PI pi; IntPtr file = IntPtr.Zero; bool inherit = false;
    if (!string.IsNullOrEmpty(log)) { var sa = new SA(); sa.n = System.Runtime.InteropServices.Marshal.SizeOf(sa); sa.inh = true;
      file = CreateFileW(log, 0x40000000, 3, ref sa, 2, 0x80, IntPtr.Zero); if (file == new IntPtr(-1)) throw new System.ComponentModel.Win32Exception();
      si.flags = 0x100; si.o = file; si.e = file; inherit = true; }
    bool ok = CreateProcessW(null, new StringBuilder(cmd), IntPtr.Zero, IntPtr.Zero, inherit, inherit_console ? 0u : 0x00000008u, IntPtr.Zero, null, ref si, out pi);
    if (file != IntPtr.Zero) CloseHandle(file);
    if (!ok) throw new System.ComponentModel.Win32Exception();
    pid = pi.pid; if (WaitForSingleObject(pi.hp, timeoutMs) != 0) return -1; uint code; GetExitCodeProcess(pi.hp, out code); return (int)code; }
}
'@
$suspect = 'ConsoleWindowClass', 'CASCADIA_HOSTING_WINDOW_CLASS', 'PseudoConsoleWindow'
$names = 'pb', 'schtasks', 'powershell', 'pwsh', 'cmd', 'conhost', 'ssh', 'sshd', 'winget', 'msiexec', 'icacls', 'netsh', 'sc', 'taskkill', 'tasklist', 'cygpath', 'rsync', 'bash', 'cat'
$seen = @{}
function Sample($base) {
  foreach ($w in [Win]::Visible()) {
    $f = $w -split '\|', 4
    if ($base.ContainsKey($f[0]) -or $seen.ContainsKey($f[0])) { continue }
    $p = Get-Process -Id ([int]$f[1]) -ErrorAction SilentlyContinue
    $seen[$f[0]] = [pscustomobject]@{ hwnd = $f[0]; pid = [int]$f[1]; process = $(if ($p) { $p.ProcessName } else { '?' }); class = $f[2]; title = $f[3] }
  }
}
$base = @{}; foreach ($w in [Win]::Visible()) { $base[($w -split '\|')[0]] = 1 }
Write-Output "baseline windows: $($base.Count)"
if ($PathPrepend) { $env:PATH = $PathPrepend + ';' + $env:PATH }  # the child inherits it
$cmd = '"' + $Exe + '" ' + $ArgumentLine
$pidOut = [uint32]0; $code = $null; $job = $null
$job = [powerShell]::Create().AddScript({ param($c, $ms, $l, $i) $p = [uint32]0; [Win]::RunDetached($c, $ms, $l, $i, [ref]$p) }).AddArgument($cmd).AddArgument([uint32](($(if ($Timeout) { $Timeout } else { $Seconds })) * 1000)).AddArgument($Log).AddArgument([bool]$Inherit)
$handle = $job.BeginInvoke()
Write-Output "started: $cmd"
$end = $null
while ($true) {
  Sample $base
  if ($handle.IsCompleted -and -not $end) { $end = (Get-Date).AddSeconds($Seconds) }
  if ($end -and (Get-Date) -gt $end) { break }
  Start-Sleep -Milliseconds 25
}
Write-Output "finished, windows seen: $($seen.Count)"
$code = $job.EndInvoke($handle)
if ($job.Streams.Error.Count) { Write-Output ('runspace error: ' + $job.Streams.Error[0]) }
$flashes = @($seen.Values | Where-Object { $suspect -contains $_.class -or $names -contains $_.process })
$result = [ordered]@{ command = $cmd; exit = ($code | Select-Object -First 1); control = [bool]$Control; flashes = $flashes }
$result | ConvertTo-Json -Depth 4 | Set-Content -Encoding UTF8 -Path $Out
if ($Control) { exit $(if ($flashes.Count -gt 0) { 0 } else { 1 }) }  # the control must show a window, else the detector is blind
exit $(if ($flashes.Count -eq 0) { 0 } else { 1 })
