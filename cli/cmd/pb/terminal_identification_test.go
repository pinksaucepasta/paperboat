package main

import (
	"github.com/pinksaucepasta/paperboat/internal/api"
	"strings"
	"testing"
)

func TestSessionIdentificationPresentation(t *testing.T) {
	s := api.TerminalSession{Name: "backend-work", Title: "✳ Fixing auth", ForegroundProcess: "claude", CurrentDirectory: "/projects/api", StartedIn: "/home/user"}
	if got := sessionIdentificationDetails(s, "hp · attached"); got != "✳ Fixing auth · /projects/api · hp · attached" {
		t.Fatalf("details=%q", got)
	}
	if s.Name != "backend-work" {
		t.Fatal("application title changed chosen name")
	}
	s.Title = ""
	if sessionActivityLabel(s) != "claude" {
		t.Fatal("foreground fallback missing")
	}
	s.CurrentDirectory = ""
	if got := sessionDirectoryLabel(s); got != "Started in /home/user" {
		t.Fatalf("launch directory mislabeled: %q", got)
	}
	s.StartedIn = ""
	s.ForegroundProcess = ""
	if sessionActivityLabel(s) != "—" || sessionDirectoryLabel(s) != "Directory unavailable" {
		t.Fatal("missing metadata guessed")
	}
	s.Title = "evil\x1b[2J\nname"
	s.CurrentDirectory = "/work\tother"
	if strings.ContainsAny(sessionIdentificationDetails(s, ""), "\x1b\n\t") {
		t.Fatal("terminal controls reached presentation")
	}
}
