package machineguard

import (
	"encoding/xml"
	"fmt"
	"strings"
)

const darwinLaunchMarker = "<!-- Managed by Paperboat machine guard -->"

// darwinGuardLaunchPlist is the machine guard's launchd job. Its output goes to /var/log/<label>.log like every
// other Paperboat launchd job (hostruntime/service), so macOS keeps what journald keeps on Linux.
func darwinGuardLaunchPlist(label string, programArguments []string) (string, error) {
	var argumentXML strings.Builder
	for _, argument := range programArguments {
		argumentXML.WriteString("<string>")
		if err := xml.EscapeText(&argumentXML, []byte(argument)); err != nil {
			return "", err
		}
		argumentXML.WriteString("</string>")
	}
	var logPath strings.Builder
	if err := xml.EscapeText(&logPath, []byte("/var/log/"+label+".log")); err != nil {
		return "", err
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
%s
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array>%s</array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>ProcessType</key><string>Background</string><key>ThrottleInterval</key><integer>2</integer>
<key>Umask</key><integer>63</integer>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, darwinLaunchMarker, label, argumentXML.String(), logPath.String(), logPath.String()), nil
}

// darwinNewsyslogPath bounds every Paperboat launchd log: newsyslog (the macOS rotation, as Homebrew services
// and Tailscale's standalone package use) rotates at 1 MB, keeps 3 and gzips them. Install writes it; purge
// removes it.
const darwinNewsyslogPath = "/etc/newsyslog.d/io.paperboat.conf"

// darwinNewsyslogConf covers the guard's log (io.paperboat.*) and the per-user jobs' logs
// (com.pinksaucepasta.paperboat.*). G: glob, Z: gzip, N: send no signal (launchd jobs write to the file that
// launchd opened; the next start opens a new one).
const darwinNewsyslogConf = `# Managed by Paperboat machine guard: rotate Paperboat launchd logs at 1 MB, keep 3, gzip.
# logfilename                                   mode count size when flags
/var/log/io.paperboat.*.log                     644  3     1024 *    GZN
/var/log/com.pinksaucepasta.paperboat.*.log     644  3     1024 *    GZN
`
