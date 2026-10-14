package containers

import (
	"bufio"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
)

func CheckHost() error {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return errors.New("read Linux operating system failed")
	}
	defer file.Close()
	return supportedHost(runtime.GOOS, runtime.GOARCH, file)
}

func supportedHost(platform, architecture string, release io.Reader) error {
	if platform != "linux" || architecture != "amd64" && architecture != "arm64" {
		return errors.New("self-host requires Linux amd64 or arm64")
	}
	var id, version string
	scanner := bufio.NewScanner(io.LimitReader(release, 16385))
	scanner.Buffer(make([]byte, 4096), 4096)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, "\"'")
		switch key {
		case "ID":
			id = value
		case "VERSION_ID":
			version = value
		}
	}
	if scanner.Err() != nil {
		return errors.New("Linux operating system metadata is invalid")
	}
	if id == "ubuntu" && (version == "22.04" || version == "24.04") || id == "debian" && version == "12" {
		return nil
	}
	return errors.New("self-host supports Ubuntu 22.04/24.04 or Debian 12")
}
