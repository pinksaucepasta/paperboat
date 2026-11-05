package machineguard

import "testing"

func TestInvokingUIDPrefersTheExplicitValueAndIgnoresInheritedSudoUnprivileged(t *testing.T) {
	env := map[string]string{"SUDO_UID": "501", "PAPERBOAT_INVOKING_UID": "502"}
	get := func(name string) string { return env[name] }
	if uid, ok := invokingUID(0, 0, get); !ok || uid != 502 {
		t.Fatalf("root: got %d, %v; want the explicit 502 over SUDO_UID 501", uid, ok)
	}
	if uid, ok := invokingUID(502, 502, get); !ok || uid != 502 {
		t.Fatalf("unprivileged: got %d, %v; want its own uid", uid, ok)
	}
	env = map[string]string{"SUDO_UID": "501"}
	if uid, ok := invokingUID(502, 502, get); !ok || uid != 502 {
		t.Fatalf("unprivileged with an inherited SUDO_UID: got %d, %v; want its own 502", uid, ok)
	}
	if uid, ok := invokingUID(0, 0, get); !ok || uid != 501 {
		t.Fatalf("root without the explicit value: got %d, %v; want SUDO_UID 501", uid, ok)
	}
	env = map[string]string{"PAPERBOAT_INVOKING_UID": "nope", "SUDO_UID": "501"}
	if _, ok := invokingUID(0, 0, get); ok {
		t.Fatal("root: an invalid explicit value must not fall back to SUDO_UID")
	}
}
