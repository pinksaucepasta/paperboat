package config

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBrowserSessionsConfiguration(t *testing.T) {
	defaults := DefaultBrowserSessions()
	if defaults.LoginLifetime != 30*24*time.Hour || defaults.LoginRotateBefore != 7*24*time.Hour || defaults.ResourceAbsoluteLifetime != 12*time.Hour || defaults.ResourceIdleLifetime != 30*time.Minute {
		t.Fatal("existing session defaults changed")
	}
	cfg, err := Load(context.Background(), LoadOptions{FilePath: "config.json", ReadFile: func(string) ([]byte, error) {
		return []byte(`{"browser_sessions":{"login_lifetime":7200000000000,"login_rotate_before":1800000000000}}`), nil
	}, LookupEnv: func(k string) (string, bool) {
		if k == "PAPERBOAT_BROWSER_SESSIONS_RESOURCE_IDLE_LIFETIME" {
			return "5m", true
		}
		return "", false
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrowserSessions.LoginLifetime != 2*time.Hour || cfg.BrowserSessions.LoginRotateBefore != 30*time.Minute || cfg.BrowserSessions.ResourceIdleLifetime != 5*time.Minute || cfg.BrowserSessions.ResourceAbsoluteLifetime != 12*time.Hour {
		t.Fatal("session config did not overlay defaults")
	}
	if err = cfg.BrowserSessions.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*BrowserSessions)
	}{
		{"zero", func(p *BrowserSessions) { p.LoginLifetime = 0 }},
		{"negative", func(p *BrowserSessions) { p.ResourceIdleLifetime = -time.Second }},
		{"fractional", func(p *BrowserSessions) { p.LoginRotateBefore = time.Millisecond }},
		{"rotation", func(p *BrowserSessions) { p.LoginRotateBefore = p.LoginLifetime }},
		{"idle", func(p *BrowserSessions) { p.ResourceIdleLifetime = p.ResourceAbsoluteLifetime + time.Second }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := defaults
			tc.change(&p)
			if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "browser_sessions") {
				t.Fatalf("validation=%v", err)
			}
		})
	}
	_, err = Load(context.Background(), LoadOptions{LookupEnv: func(k string) (string, bool) {
		if k == "PAPERBOAT_BROWSER_SESSIONS_LOGIN_LIFETIME" {
			return "forever", true
		}
		return "", false
	}})
	if err == nil {
		t.Fatal("invalid environment duration accepted")
	}
}
