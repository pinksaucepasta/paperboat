package localdaemon

import "testing"

func TestWindowsOpenSSHExecutableIsOneFormBothSSHFlavoursAccept(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Program Files\Paperboat\users\u1\bin\pb.exe`: `//?/C:/Program Files/Paperboat/users/u1/bin/pb.exe`,
		`c:/Program Files/Paperboat/bin/pb.exe`:          `//?/c:/Program Files/Paperboat/bin/pb.exe`,
		`/opt/paperboat/pb`:                              `/opt/paperboat/pb`,
		`\\server\share\pb.exe`:                          `//server/share/pb.exe`,
	} {
		if got := windowsOpenSSHExecutable(in); got != want {
			t.Errorf("windowsOpenSSHExecutable(%q) = %q, want %q", in, got, want)
		}
	}
}
