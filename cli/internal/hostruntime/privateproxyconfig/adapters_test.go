package privateproxyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type call struct {
	name string
	args []string
}
type scriptedRunner struct {
	outputs [][]byte
	errs    []error
	calls   []call
}

func (s *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.calls = append(s.calls, call{name, append([]string(nil), args...)})
	i := len(s.calls) - 1
	var out []byte
	var err error
	if i < len(s.outputs) {
		out = s.outputs[i]
	}
	if i < len(s.errs) {
		err = s.errs[i]
	}
	return out, err
}

func TestMacSnapshotUsesExactArgvAndSkipsDisabled(t *testing.T) {
	r := &scriptedRunner{outputs: [][]byte{[]byte("An asterisk denotes disabled services.\nWi-Fi\n*VPN\nEthernet\n"), []byte("URL: http://old/p.pac\nEnabled: Yes\n"), []byte("URL: (null)\nEnabled: No\n")}}
	a := NewMacOSAdapter(r)
	raw, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got macState
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Services) != 2 || got.Services[0].URL != "http://old/p.pac" || got.Services[1].URL != "" || got.Services[1].Enabled {
		t.Fatalf("state=%+v", got)
	}
	want := call{networksetup, []string{"-getautoproxyurl", "Wi-Fi"}}
	if !reflect.DeepEqual(r.calls[1], want) {
		t.Fatalf("call=%+v", r.calls[1])
	}
}

func TestMacMatchesIgnoresRetainedURLOnlyWhenProxyIsDisabled(t *testing.T) {
	want, _ := json.Marshal(macState{Services: []macService{{Name: "Wi-Fi", Enabled: false}}})
	r := &scriptedRunner{outputs: [][]byte{[]byte("Wi-Fi\n"), []byte("URL: http://127.0.0.1:56837/proxy.pac\nEnabled: No\n")}}
	matched, err := NewMacOSAdapter(r).Matches(context.Background(), want)
	if err != nil || !matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}

	want, _ = json.Marshal(macState{Services: []macService{{Name: "Wi-Fi", URL: "http://127.0.0.1:1/proxy.pac", Enabled: true}}})
	r = &scriptedRunner{outputs: [][]byte{[]byte("Wi-Fi\n"), []byte("URL: http://127.0.0.1:2/proxy.pac\nEnabled: Yes\n")}}
	matched, err = NewMacOSAdapter(r).Matches(context.Background(), want)
	if err != nil || matched {
		t.Fatalf("matched=%v err=%v", matched, err)
	}
}

func TestLinuxRequiresExplicitGNOMESession(t *testing.T) {
	r := &scriptedRunner{}
	a := NewLinuxAdapter(r, func(string) string { return "" })
	if _, err := a.Snapshot(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err=%v", err)
	}
	if len(r.calls) != 0 {
		t.Fatal("mutated without supported session")
	}
}

type fakeRegistry struct {
	interactive bool
	value       RegistryValue
	broadcasts  int
}

func (r *fakeRegistry) InteractiveUser(context.Context) (bool, error)           { return r.interactive, nil }
func (r *fakeRegistry) GetAutoConfigURL(context.Context) (RegistryValue, error) { return r.value, nil }
func (r *fakeRegistry) SetAutoConfigURL(_ context.Context, v RegistryValue) error {
	r.value = v
	return nil
}
func (r *fakeRegistry) BroadcastInternetSettingsChanged(context.Context) error {
	r.broadcasts++
	return nil
}
func TestWindowsPreservesMissingValueAndRejectsSystem(t *testing.T) {
	r := &fakeRegistry{interactive: true}
	a := NewWindowsAdapter(r)
	raw, err := a.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Install(context.Background(), "http://127.0.0.1:1/a.pac"); err != nil {
		t.Fatal(err)
	}
	if err := a.Restore(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if r.value.Exists || r.broadcasts != 2 {
		t.Fatalf("registry=%+v broadcasts=%d", r.value, r.broadcasts)
	}
	r.interactive = false
	if _, err := a.Snapshot(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestMacRefreshTransitionOwnershipRejectsExternalInterface(t *testing.T) {
	for _, external := range []bool{false, true} {
		last := "http://127.0.0.1:99/new.pac"
		if external {
			last = "http://external/pac"
		}
		r := &scriptedRunner{outputs: [][]byte{[]byte("Wi-Fi\nEthernet\n"), []byte("URL: http://127.0.0.1:99/old.pac\nEnabled: Yes\n"), []byte("URL: " + last + "\nEnabled: Yes\n")}}
		owned, err := NewMacOSAdapter(r).OwnsTransition(context.Background(), "http://127.0.0.1:99/old.pac", "http://127.0.0.1:99/new.pac")
		if err != nil || owned == external {
			t.Fatalf("external=%v owned=%v err=%v", external, owned, err)
		}
	}
}

// macMutationRunner models two network interfaces and a failure after the first
// interface changes, so Manager exercises the real adapter's partial writes.
type macMutationRunner struct {
	services []macService
	failURL  string
	external bool
}

func (r *macMutationRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if args[0] == "-listallnetworkservices" {
		return []byte("Wi-Fi\nEthernet\n"), nil
	}
	for i := range r.services {
		s := &r.services[i]
		if s.Name != args[1] {
			continue
		}
		switch args[0] {
		case "-getautoproxyurl":
			enabled := "No"
			if s.Enabled {
				enabled = "Yes"
			}
			return []byte("URL: " + s.URL + "\nEnabled: " + enabled + "\n"), nil
		case "-setautoproxyurl":
			if s.Name == "Ethernet" && args[2] == r.failURL {
				if r.external {
					s.URL = "http://user.example/proxy.pac"
				}
				return nil, errors.New("network service write failed")
			}
			s.URL = args[2]
		case "-setautoproxystate":
			s.Enabled = args[2] == "on"
		}
		return nil, nil
	}
	return nil, errors.New("unknown network service")
}

func TestManagerMacPartialRefreshRollbackAndExternalChange(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "external"}[external], func(t *testing.T) {
			ctx := context.Background()
			prior := []macService{{Name: "Wi-Fi", URL: "http://prior.example/a", Enabled: true}, {Name: "Ethernet", URL: "http://prior.example/b", Enabled: false}}
			r := &macMutationRunner{services: append([]macService(nil), prior...)}
			m, _ := New(filepath.Join(t.TempDir(), "j"), NewMacOSAdapter(r))
			first, second := "http://127.0.0.1:99/first.pac", "http://127.0.0.1:99/second.pac"
			if err := m.Install(ctx, first); err != nil {
				t.Fatal(err)
			}
			r.failURL, r.external = second, external
			err := m.Refresh(ctx, second)
			if err == nil {
				t.Fatal("partial write unexpectedly succeeded")
			}
			if external {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("refresh=%v", err)
				}
				if r.services[0].URL != second || r.services[1].URL != "http://user.example/proxy.pac" {
					t.Fatalf("external change overwritten: %+v", r.services)
				}
				if err := m.Recover(ctx); !errors.Is(err, ErrConflict) {
					t.Fatalf("recover=%v", err)
				}
				return
			}
			for _, s := range r.services {
				if !s.Enabled || s.URL != first {
					t.Fatalf("failed refresh lost previous PAC: %+v", r.services)
				}
			}
			r.failURL = ""
			if err := m.Refresh(ctx, second); err != nil {
				t.Fatal(err)
			}
			if err := m.Remove(ctx); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.services, prior) {
				t.Fatalf("original network settings lost: %+v", r.services)
			}
		})
	}
}

func TestManagerMacInterruptedOriginalRestorationRecovery(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "external"}[external], func(t *testing.T) {
			ctx := context.Background()
			prior := []macService{{Name: "Wi-Fi", URL: "http://prior.example/a", Enabled: false}, {Name: "Ethernet", URL: "http://prior.example/b", Enabled: true}}
			r := &macMutationRunner{services: append([]macService(nil), prior...)}
			path := filepath.Join(t.TempDir(), "j")
			m, _ := New(path, NewMacOSAdapter(r))
			first, second := "http://127.0.0.1:99/first.pac", "http://127.0.0.1:99/second.pac"
			if err := m.Install(ctx, first); err != nil {
				t.Fatal(err)
			}
			if err := m.Refresh(ctx, second); err != nil {
				t.Fatal(err)
			}
			r.failURL = prior[1].URL
			if err := m.Remove(ctx); err == nil {
				t.Fatal("expected interrupted original restoration")
			}
			if r.services[0] != prior[0] || r.services[1].URL != second {
				t.Fatalf("did not exercise partial original restore: %+v", r.services)
			}
			j, err := m.read()
			if err != nil || j.Phase != "restoring" {
				t.Fatalf("journal=%+v err=%v", j, err)
			}
			restarted, _ := New(path, NewMacOSAdapter(r))
			r.failURL = ""
			if external {
				r.services[1].URL = "http://user.example/changed"
				before := append([]macService(nil), r.services...)
				if err := restarted.Recover(ctx); !errors.Is(err, ErrConflict) {
					t.Fatalf("recover=%v", err)
				}
				if !reflect.DeepEqual(r.services, before) {
					t.Fatalf("external state overwritten: %+v", r.services)
				}
				return
			}
			if err := restarted.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.services, prior) {
				t.Fatalf("original proxy state lost: %+v", r.services)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("restoration journal remained: %v", err)
			}
		})
	}
}
