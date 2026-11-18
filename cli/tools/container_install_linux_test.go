package tools

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The installer installs OpenSSH itself on images that lack ssh-keygen (apt, apk, dnf). It pulls images and packages,
// so it runs only when asked: PB_CONTAINER_TESTS=1 (DOCKER="sudo docker" if docker needs it).
func TestInstallerInstallsOpenSSHInMinimalContainers(t *testing.T) {
	if os.Getenv("PB_CONTAINER_TESTS") != "1" {
		t.Skip("set PB_CONTAINER_TESTS=1 to pull images and run the container installs")
	}
	docker := os.Getenv("DOCKER")
	if docker == "" {
		docker = "docker"
	}
	if err := exec.Command("sh", "-c", docker+" info >/dev/null 2>&1").Run(); err != nil {
		t.Skip("docker is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "test-install-container.sh")
	command.Env = append(os.Environ(), "DOCKER="+docker)
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "container installs: ok") {
		t.Fatalf("container installs: %v\n%s", err, output)
	}
}
