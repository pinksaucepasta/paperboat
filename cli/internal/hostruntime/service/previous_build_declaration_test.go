package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The declarations below are what official 2026.11.19.0 published (rendered by that tag's renderSystemd/renderLaunchd for these
// configs). Installer.VerifyPublishedDefinition of the next build compared bytes, so every install over them failed "invalid
// service definition" once 4c188a9f6 changed the systemd watchdog limits and 252b2ada1 the Windows labels (victus, 2026-11-19).
const (
	previousSystemdWorker = "[Unit]\nDescription=Paperboat host runtime\nAfter=local-fs.target\nStartLimitIntervalSec=0\n\n[Service]\nType=notify\nUser=paperboat\nGroup=paperboat\nExecStart=\"/usr/local/libexec/paperboat/pb\"\nRestart=always\nRestartSec=5s\nRestartSteps=3\nRestartMaxDelaySec=30s\nTimeoutStopSec=60s\nKillMode=mixed\nUMask=0077\nLimitCORE=0\nNotifyAccess=main\nWatchdogSec=30s\nRuntimeDirectory=paperboat\nRuntimeDirectoryMode=0755\nStateDirectory=paperboat\nStateDirectoryMode=0700\nCacheDirectory=paperboat\nCacheDirectoryMode=0750\nNoNewPrivileges=false\nPrivateTmp=true\n\n[Install]\nWantedBy=multi-user.target\n"
	previousSystemdHostd  = "[Unit]\nDescription=Paperboat stable host supervisor\nAfter=local-fs.target\nStartLimitIntervalSec=0\n\n[Service]\nType=notify\nUser=paperboat\nGroup=paperboat\nExecStart=\"/usr/local/libexec/paperboat/pb\" \"daemon\" \"__runtime-hostd\"\nRestart=on-failure\nRestartSec=5s\nRestartSteps=3\nRestartMaxDelaySec=30s\nTimeoutStopSec=60s\nKillMode=mixed\nUMask=0077\nLimitCORE=0\nNotifyAccess=main\nWatchdogSec=30s\nRuntimeDirectoryPreserve=yes\nRuntimeDirectory=paperboat-hostd\nRuntimeDirectoryMode=0700\nStateDirectory=paperboat-hostd\nStateDirectoryMode=0700\nCacheDirectory=paperboat-hostd\nCacheDirectoryMode=0750\nNoNewPrivileges=false\nPrivateTmp=true\n\n[Install]\nWantedBy=multi-user.target\n"
	previousLaunchdDaemon = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n  <dict>\n    <key>EnvironmentVariables</key>\n    <dict>\n    </dict>\n    <key>KeepAlive</key>\n    <true/>\n    <key>Label</key>\n    <string>com.pinksaucepasta.paperboatd</string>\n    <key>ProcessType</key>\n    <string>Background</string>\n    <key>ProgramArguments</key>\n    <array>\n      <string>/Users/t/.local/bin/pb</string>\n      <string>daemon</string>\n    </array>\n    <key>RunAtLoad</key>\n    <true/>\n    <key>StandardErrorPath</key>\n    <string>/Users/t/Library/Logs/Paperboat/paperboatd.log</string>\n    <key>StandardOutPath</key>\n    <string>/Users/t/Library/Logs/Paperboat/paperboatd.log</string>\n    <key>Umask</key>\n    <integer>63</integer>\n  </dict>\n</plist>"
	previousLaunchdHostd  = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n  <dict>\n    <key>EnvironmentVariables</key>\n    <dict>\n    </dict>\n    <key>GroupName</key>\n    <string>wheel</string>\n    <key>KeepAlive</key>\n    <dict>\n      <key>SuccessfulExit</key>\n      <false/>\n    </dict>\n    <key>Label</key>\n    <string>com.pinksaucepasta.paperboat.hostd</string>\n    <key>ProcessType</key>\n    <string>Background</string>\n    <key>ProgramArguments</key>\n    <array>\n      <string>/usr/local/bin/pb</string>\n      <string>daemon</string>\n      <string>__runtime-hostd</string>\n    </array>\n    <key>RunAtLoad</key>\n    <true/>\n    <key>StandardErrorPath</key>\n    <string>/var/log/com.pinksaucepasta.paperboat.hostd.log</string>\n    <key>StandardOutPath</key>\n    <string>/var/log/com.pinksaucepasta.paperboat.hostd.log</string>\n    <key>Umask</key>\n    <integer>63</integer>\n    <key>UserName</key>\n    <string>root</string>\n  </dict>\n</plist>"
)

func publishedBy(t *testing.T, config Config, previous string) *Installer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "declaration")
	if err := os.WriteFile(path, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Installer{config: config, definitionPath: path}
}

func TestLinuxNewBuildVerifiesTheDeclarationThePreviousBuildPublished(t *testing.T) {
	for name, test := range map[string]struct {
		config   Config
		previous string
	}{
		"worker": {Config{Platform: "linux", Kind: WorkerKind, User: "paperboat", Group: "paperboat", Executable: "/usr/local/libexec/paperboat/pb"}, previousSystemdWorker},
		"hostd":  {Config{Platform: "linux", Kind: HostdKind, User: "paperboat", Group: "paperboat", Executable: "/usr/local/libexec/paperboat/pb", Arguments: []string{"daemon", "__runtime-hostd"}}, previousSystemdHostd},
	} {
		t.Run(name, func(t *testing.T) {
			if err := publishedBy(t, test.config, test.previous).VerifyPublishedDefinition(); err != nil {
				t.Fatalf("the previous build's unit was refused: %v", err)
			}
			// What the unit runs is still checked.
			other := strings.Replace(test.previous, "ExecStart=\"/usr/local/libexec/paperboat/pb\"", "ExecStart=\"/tmp/evil\"", 1)
			if err := publishedBy(t, test.config, other).VerifyPublishedDefinition(); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("a unit for another executable was accepted: %v", err)
			}
		})
	}
}

func TestMacOSNewBuildVerifiesTheDeclarationThePreviousBuildPublished(t *testing.T) {
	for name, test := range map[string]struct {
		config   Config
		previous string
	}{
		"daemon": {Config{Platform: "darwin", Kind: DaemonKind, ConfigRoot: "/Users/t", Executable: "/Users/t/.local/bin/pb", User: "t", Group: "staff", Arguments: []string{"daemon"}}, previousLaunchdDaemon},
		"hostd":  {Config{Platform: "darwin", Kind: HostdKind, Executable: "/usr/local/bin/pb", User: "root", Group: "wheel", Arguments: []string{"daemon", "__runtime-hostd"}}, previousLaunchdHostd},
	} {
		t.Run(name, func(t *testing.T) {
			if err := publishedBy(t, test.config, test.previous).VerifyPublishedDefinition(); err != nil {
				t.Fatalf("the previous build's plist was refused: %v", err)
			}
			other := strings.Replace(test.previous, "<string>daemon</string>", "<string>other</string>", 1)
			if err := publishedBy(t, test.config, other).VerifyPublishedDefinition(); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("a plist for other arguments was accepted: %v", err)
			}
		})
	}
}
