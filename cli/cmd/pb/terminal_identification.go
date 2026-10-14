package main

import (
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/selector"
	"strings"
)

func sessionActivityLabel(session api.TerminalSession) string {
	if title := strings.TrimSpace(selector.SanitizeText(session.Title)); title != "" {
		return title
	}
	if process := strings.TrimSpace(selector.SanitizeText(session.ForegroundProcess)); process != "" {
		return process
	}
	return "—"
}

func sessionDirectoryLabel(session api.TerminalSession) string {
	if directory := strings.TrimSpace(selector.SanitizeText(session.CurrentDirectory)); directory != "" {
		return directory
	}
	if directory := strings.TrimSpace(selector.SanitizeText(session.StartedIn)); directory != "" {
		return "Started in " + directory
	}
	return "Directory unavailable"
}

func sessionIdentificationDetails(session api.TerminalSession, rest string) string {
	details := []string{sessionActivityLabel(session), sessionDirectoryLabel(session)}
	if rest != "" {
		details = append(details, rest)
	}
	return strings.Join(details, " · ")
}

func sessionIdentificationSearch(session api.TerminalSession) string {
	return selector.SanitizeText(strings.Join([]string{session.Title, session.ForegroundProcess, session.CurrentDirectory, session.StartedIn}, " "))
}
