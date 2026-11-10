package main

import "testing"

func TestRequestsAgentForwarding(t *testing.T) {
	for name, test := range map[string]struct {
		args []string
		want bool
	}{
		"-A":                          {[]string{"-A"}, true},
		"-A with a command":           {[]string{"-A", "ssh-add", "-l"}, true},
		"clustered":                   {[]string{"-tAv"}, true},
		"-o ForwardAgent=yes":         {[]string{"-o", "ForwardAgent=yes"}, true},
		"-oForwardAgent=yes":          {[]string{"-oForwardAgent=yes"}, true},
		"-o ForwardAgent yes":         {[]string{"-o", "ForwardAgent yes"}, true},
		"case-insensitive":            {[]string{"-o", "forwardagent=YES"}, true},
		"-a turns it off":             {[]string{"-A", "-a"}, false},
		"ForwardAgent=no":             {[]string{"-A", "-o", "ForwardAgent=no"}, false},
		"-A after the command":        {[]string{"echo", "-A"}, false},
		"-A is a value of -L":         {[]string{"-L", "1:h:2", "uname"}, false},
		"-p value looks like A":       {[]string{"-pA"}, false},
		"-i value looks like A":       {[]string{"-i", "-A"}, false},
		"plain command":               {[]string{"ls"}, false},
		"nothing":                     {nil, false},
		"-- ends the options":         {[]string{"--", "-A"}, false},
		"other -o":                    {[]string{"-o", "StrictHostKeyChecking=yes"}, false},
		"-v alone":                    {[]string{"-v"}, false},
		"cluster before a value flag": {[]string{"-Ap", "22"}, true},
	} {
		if got := requestsAgentForwarding(test.args); got != test.want {
			t.Errorf("%s: requestsAgentForwarding(%q) = %v, want %v", name, test.args, got, test.want)
		}
	}
}

func TestAgentForwardingNoticeIsOnlyForWindowsMachines(t *testing.T) {
	for platform, want := range map[string]string{"windows": sshWindowsAgentForwardingMessage, "Windows": sshWindowsAgentForwardingMessage, "linux": "", "darwin": "", "": ""} {
		if got := agentForwardingNotice(platform, []string{"-A", "hostname"}); got != want {
			t.Errorf("%q with -A: %q, want %q", platform, got, want)
		}
	}
	if got := agentForwardingNotice("windows", []string{"-L", "1:h:2", "hostname"}); got != "" {
		t.Errorf("no agent requested but printed %q", got)
	}
}
