package config

import (
	"fmt"
	"time"
)

// BrowserSessions is startup policy. Existing persisted deadlines are not rewritten.
type BrowserSessions struct {
	LoginLifetime            time.Duration `json:"login_lifetime"`
	LoginRotateBefore        time.Duration `json:"login_rotate_before"`
	ResourceAbsoluteLifetime time.Duration `json:"resource_absolute_lifetime"`
	ResourceIdleLifetime     time.Duration `json:"resource_idle_lifetime"`
}

func DefaultBrowserSessions() BrowserSessions {
	return BrowserSessions{30 * 24 * time.Hour, 7 * 24 * time.Hour, 12 * time.Hour, 30 * time.Minute}
}

func (p BrowserSessions) Validate() error {
	for name, value := range map[string]time.Duration{"login_lifetime": p.LoginLifetime, "login_rotate_before": p.LoginRotateBefore, "resource_absolute_lifetime": p.ResourceAbsoluteLifetime, "resource_idle_lifetime": p.ResourceIdleLifetime} {
		if value <= 0 || value%time.Second != 0 {
			return fmt.Errorf("browser_sessions.%s must be a positive whole-second duration", name)
		}
	}
	if p.LoginRotateBefore >= p.LoginLifetime {
		return fmt.Errorf("browser_sessions.login_rotate_before must be shorter than login_lifetime")
	}
	if p.ResourceIdleLifetime > p.ResourceAbsoluteLifetime {
		return fmt.Errorf("browser_sessions.resource_idle_lifetime must not exceed resource_absolute_lifetime")
	}
	return nil
}
