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
