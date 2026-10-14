//go:build windows

package process

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// These values describe the enrolled owner's native Windows process context.
// They are not client overrides and never include service/control credentials.
var windowsEnvironmentKeys = map[string]bool{
	"USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true,
	"HOMEDRIVE": true, "HOMEPATH": true, "USERNAME": true, "USERDOMAIN": true,
	"SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true, "TEMP": true, "TMP": true,
	"PROMPT": true, "PATHEXT": true, "PROGRAMDATA": true, "ALLUSERSPROFILE": true,
}

func platformEnvironmentKey(key string) bool { return windowsEnvironmentKeys[key] }

func BaseEnvironment(shell string) ([]string, error) {
	token := windows.GetCurrentProcessToken()
	values := map[string]string{"SHELL": shell, "TERM": "xterm-256color"}
	for key := range windowsEnvironmentKeys {
		if value := os.Getenv(key); value != "" {
			values[key] = value
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
		values[folder.key] = value
	}
	values["HOME"] = values["USERPROFILE"]
	values["HOMEDRIVE"] = filepath.VolumeName(values["USERPROFILE"])
	values["HOMEPATH"] = strings.TrimPrefix(values["USERPROFILE"], values["HOMEDRIVE"])
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) {
		return nil, ErrLaunchRejected
	}
	// The installed per-user executable is not necessarily on the SCM-created
	// token's PATH. Expose this same trusted runtime binary to its owner's shell.
	binaryDirectory := filepath.Dir(executable)
	path := os.Getenv("PATH")
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
	values["PATH"] = path
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(values))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	if !validEnvironment(result) {
		return nil, ErrLaunchRejected
	}
	return result, nil
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
