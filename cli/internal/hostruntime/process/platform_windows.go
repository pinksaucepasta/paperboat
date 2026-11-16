//go:build windows

package process

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

func platformExecutable(path string, info fs.FileInfo) bool {
	if !strings.EqualFold(filepath.Ext(info.Name()), ".exe") {
		return false
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	return err == nil && attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

func platformShellArguments(path string) []string {
	switch strings.ToLower(filepath.Base(path)) {
	case "powershell.exe", "pwsh.exe":
		return []string{"-NoLogo", "-NoExit", "-Command", paperboatPowerShellPrompt}
	case "cmd.exe":
		return []string{"/d"}
	default:
		return nil
	}
}

// Environment names the owner context owns: a client can never override them, and hostd's own PAPERBOAT_*
// variables (service/control credential references) never reach a session. Everything else in the owner's
// environment block is passed through, as Windows does for a logged-on user.
func platformEnvironmentKey(key string) bool {
	return key != "" && !strings.HasPrefix(strings.ToUpper(key), "PAPERBOAT_")
}

// BaseEnvironment is the owner's environment the way Windows builds it at logon: CreateEnvironmentBlock for the
// process token without inheriting hostd's own variables (the same call Win32-OpenSSH and runas use), plus the
// standard variables an S4U/service-created token does not receive from it.
func BaseEnvironment(shell string) ([]string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, ErrLaunchRejected
	}
	defer token.Close()
	block, err := token.Environ(false)
	if err != nil {
		return nil, ErrLaunchRejected
	}
	values := map[string]string{} // upper-cased name -> "Name=value", Windows names are case-insensitive
	set := func(name, value string) { values[strings.ToUpper(name)] = name + "=" + value }
	setDefault := func(name, value string) {
		if _, ok := values[strings.ToUpper(name)]; !ok && value != "" {
			set(name, value)
		}
	}
	for _, entry := range block {
		if name, _, ok := strings.Cut(entry, "="); ok && name != "" {
			values[strings.ToUpper(name)] = entry
		}
	}
	for _, folder := range []struct {
		key string
		id  *windows.KNOWNFOLDERID
	}{
		{"USERPROFILE", windows.FOLDERID_Profile}, {"APPDATA", windows.FOLDERID_RoamingAppData}, {"LOCALAPPDATA", windows.FOLDERID_LocalAppData},
	} {
		value, err := token.KnownFolderPath(folder.id, windows.KF_FLAG_DEFAULT)
		if err != nil || !filepath.IsAbs(value) {
			return nil, ErrLaunchRejected
		}
		set(folder.key, value)
	}
	home := strings.SplitN(values["USERPROFILE"], "=", 2)[1]
	set("HOME", home)
	set("HOMEDRIVE", filepath.VolumeName(home))
	set("HOMEPATH", strings.TrimPrefix(home, filepath.VolumeName(home)))
	set("SHELL", shell)
	set("TERM", "xterm-256color")
	for key, value := range standardWindowsEnvironment(token) {
		setDefault(key, value)
	}
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) {
		return nil, ErrLaunchRejected
	}
	// The installed per-user executable is not necessarily on the owner's PATH. Expose this same trusted
	// runtime binary to its owner's shell.
	binaryDirectory := filepath.Dir(executable)
	path := ""
	if entry, ok := values["PATH"]; ok {
		path = strings.SplitN(entry, "=", 2)[1]
	}
	found := false
	for _, directory := range filepath.SplitList(path) {
		if strings.EqualFold(filepath.Clean(directory), binaryDirectory) {
			found = true
			break
		}
	}
	if !found {
		if path != "" {
			path += ";"
		}
		path += binaryDirectory
	}
	setPath := "Path"
	if entry, ok := values["PATH"]; ok {
		setPath = strings.SplitN(entry, "=", 2)[0]
	}
	set(setPath, path)
	result := make([]string, 0, len(values))
	for _, entry := range values {
		result = append(result, entry)
	}
	sort.Strings(result)
	if !validEnvironment(result) {
		return nil, ErrLaunchRejected
	}
	return result, nil
}

// standardWindowsEnvironment returns the variables winlogon puts in an interactive session's environment that
// CreateEnvironmentBlock does not guarantee for an S4U or service-created token.
func standardWindowsEnvironment(token windows.Token) map[string]string {
	values := map[string]string{"OS": "Windows_NT", "NUMBER_OF_PROCESSORS": strconv.Itoa(runtime.NumCPU())}
	switch runtime.GOARCH {
	case "amd64":
		values["PROCESSOR_ARCHITECTURE"] = "AMD64"
	case "arm64":
		values["PROCESSOR_ARCHITECTURE"] = "ARM64"
	}
	if directory, err := windows.GetSystemDirectory(); err == nil {
		values["SystemDrive"] = filepath.VolumeName(directory)
	}
	if name, err := windows.ComputerName(); err == nil {
		values["COMPUTERNAME"] = name
	}
	// An S4U token's environment block carries no account name.
	if user, err := token.GetTokenUser(); err == nil {
		if account, domain, _, err := user.User.Sid.LookupAccount(""); err == nil {
			values["USERNAME"], values["USERDOMAIN"] = account, domain
		}
	}
	for _, folder := range []struct {
		key string
		id  *windows.KNOWNFOLDERID
	}{
		{"ProgramFiles", windows.FOLDERID_ProgramFiles}, {"ProgramW6432", windows.FOLDERID_ProgramFiles},
		{"ProgramFiles(x86)", windows.FOLDERID_ProgramFilesX86},
		{"CommonProgramFiles", windows.FOLDERID_ProgramFilesCommon}, {"CommonProgramW6432", windows.FOLDERID_ProgramFilesCommon},
		{"CommonProgramFiles(x86)", windows.FOLDERID_ProgramFilesCommonX86},
		{"ProgramData", windows.FOLDERID_ProgramData}, {"PUBLIC", windows.FOLDERID_Public},
		{"SystemRoot", windows.FOLDERID_Windows}, {"windir", windows.FOLDERID_Windows},
	} {
		if value, err := token.KnownFolderPath(folder.id, windows.KF_FLAG_DEFAULT); err == nil && filepath.IsAbs(value) {
			values[folder.key] = value
		}
	}
	return values
}

// Install a session-local prompt after normal profiles have loaded. Preserve the
// user's prompt and never write profile files or inspect another process's memory.
const paperboatPowerShellPrompt = `$global:__paperboatOriginalPrompt = (Get-Item Function:\prompt).ScriptBlock; function global:prompt { [Console]::Write(([char]27).ToString()+"]2;"+[char]7); $text = & $global:__paperboatOriginalPrompt; $location = Get-Location; if ($location.Provider.Name -eq 'FileSystem') { try { $uri = ([Uri]$location.ProviderPath).AbsoluteUri; [Console]::Write(([char]27).ToString()+"]7;"+$uri+[char]7) } catch {} } else { [Console]::Write(([char]27).ToString()+"]9;9;"+[char]7) }; $text }`

func platformShellEnvironment(path string, environment []string) []string {
	if !strings.EqualFold(filepath.Base(path), "cmd.exe") {
		return environment
	}
	prompt := "$P$G"
	for _, entry := range environment {
		if key, value, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "PROMPT") {
			prompt = value
		}
	}
	// $E is CMD's escape token; $P expands the actual directory at each prompt.
	return replaceEnvironment(environment, "PROMPT", "$E]2;\x07$E]9;9;$P\x07"+prompt)
}
