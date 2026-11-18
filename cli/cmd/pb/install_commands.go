package main

import (
	"fmt"
	"net/url"
	"strings"
)

// installerBase is the one install address from server discovery: it serves install.sh to curl and install.ps1 to
// PowerShell (the server itself serves neither; the address redirects to the scripts on GitHub).
func installerBase(installerBaseURL string) (string, error) {
	base, err := url.Parse(installerBaseURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", fmt.Errorf("server discovery does not provide a valid installer URL; ask the server operator to configure installation")
	}
	return strings.TrimRight(base.String(), "/"), nil
}

// posixInstallCommand is the Linux/macOS one-liner. Variables go on `sh`, the reader of the script, not on curl.
func posixInstallCommand(base string, variables ...[2]string) string {
	command := "curl -fsSL " + base + " | "
	for _, v := range variables {
		command += v[0] + "='" + v[1] + "' "
	}
	return command + "sh"
}

// windowsInstallCommand is the PowerShell one-liner (it is not for Command Prompt: it sets $env: variables).
func windowsInstallCommand(base string, variables ...[2]string) string {
	command := ""
	for _, v := range variables {
		command += "$env:" + v[0] + "='" + v[1] + "'; "
	}
	return command + "irm " + base + " | iex"
}

// enrollmentVariables are the environment variables that carry an enrollment token (and optional machine name) to the installers.
func enrollmentVariables(token, name string) [][2]string {
	variables := [][2]string{{"PB_TOKEN", token}}
	if name != "" {
		variables = append(variables, [2]string{"PB_NAME", name})
	}
	return variables
}
