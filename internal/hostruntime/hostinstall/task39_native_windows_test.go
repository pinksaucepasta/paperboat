//go:build windows

package hostinstall

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseindex"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"golang.org/x/sys/windows"
)

var task39WindowsPrepare = flag.Bool("task39-windows-prepare", false, "prepare a disposable Windows enrollment")
var task39WindowsOrigin = flag.String("task39-windows-origin", "", "signed fixture origin")
var task39WindowsUser = flag.String("task39-windows-user", "", "disposable Windows account")
var task39WindowsHome = flag.String("task39-windows-home", "", "disposable Windows profile")
var task39WindowsAction = flag.String("task39-windows-action", "", "privileged Task39 lifecycle action")
var task39WindowsRequests = flag.String("task39-windows-requests", "", "comma-separated Task39 request files")
var task39WindowsRefreshRequest = flag.String("task39-windows-refresh-request", "", "existing Task39 request to refresh from signed metadata")
var task39WindowsRefreshVersion = flag.String("task39-windows-refresh-version", "", "signed version expected by the refreshed request")

type task39WindowsTransport struct{ base *url.URL }

func (t task39WindowsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Hostname() != "github.com" {
		return http.DefaultTransport.RoundTrip(request)
	}
	clone := request.Clone(request.Context())
	u := *request.URL
	u.Scheme, u.Host = t.base.Scheme, t.base.Host
	u.Path = strings.TrimRight(t.base.Path, "/") + request.URL.Path
	clone.URL, clone.Host = &u, ""
	return http.DefaultTransport.RoundTrip(clone)
}

// TestTask39PrepareWindowsUser creates independent enrollment material for one
// disposable account. The orchestrator runs this entry under that account's
// token, so all private keys originate in and remain owned by that user.
func TestTask39PrepareWindowsUser(t *testing.T) {
	if !*task39WindowsPrepare && os.Getenv("PAPERBOAT_TASK39_PREPARE_WINDOWS") != "1" {
		t.Skip("native disposable-user fixture")
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	ownerSID, err := currentWindowsSID()
	if err != nil {
		t.Fatal(err)
	}
	instance, err := service.WindowsUserInstance(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := WindowsLayoutForInstance(instance)
	if err != nil {
		t.Fatal(err)
	}
	sshConfig := windowsOpenSSHConfig(layout, ownerSID)
	username := filepath.Base(account.Username)
	if *task39WindowsUser != "" {
		username = *task39WindowsUser
	}
	home := account.HomeDir
	if *task39WindowsHome != "" {
		home = *task39WindowsHome
	}
	root := filepath.Join(home, ".paperboat-task39")
	state, err := helperconfig.DefaultStateRoot(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err = os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := identity.Open(identity.Config{StateRoot: state})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	machine := "task39_machine_" + username
	control := strings.TrimRight(*task39WindowsOrigin, "/")
	if control == "" {
		control = strings.TrimRight(os.Getenv("PAPERBOAT_TASK39_ORIGIN"), "/")
	}
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerURL, cfg.Auth.AllowFileFallback = control, true
	if err = cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if configPath, pathErr := config.DefaultPath(); pathErr == nil {
		_ = os.WriteFile(filepath.Join(root, "config-path.txt"), []byte(configPath), 0600)
	}
	profiles, err := config.ProfileStoreFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	profile := config.Profile{Issuer: control, Account: config.Account{ID: "task39_account_" + username}, CLIClientSessionID: "task39_session_" + username, AccessExpiresAt: time.Now().UTC().Add(24 * time.Hour)}
	cred := config.Credential{AccessToken: "task39_access_" + username, RefreshToken: "task39_refresh_" + username, TokenType: "Bearer", ExpiresAt: time.Now().UTC().Add(24 * time.Hour)}
	if err = profiles.Save(profile, cred); err != nil {
		if err = profiles.Replace(profile, cred); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.SaveRegistration(identity.Registration{ServerURL: control, AccountID: "task39_account_" + username, MachineID: machine, EnvironmentID: "task39_env_" + username, PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: workspace, SSHUser: username, SSHPort: sshConfig.Port, InstallationGeneration: 1, SetupMode: "host", SetupRoles: []string{"host"}, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := store.PeerEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(private)
	certificate, err := endpointidentity.Sign(private, endpointidentity.Claims{AccountID: "task39_account_" + username, Role: endpointidentity.RoleMachine, EndpointID: machine, NoisePublicKey: endpoint.NoisePublicKey(), QUICPublicKey: endpoint.QUICPublicKey(), Generation: 1, Serial: 1, IssuedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := certificate.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SavePeerEndpointCertificate(public, encoded, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	credential := make([]byte, 48)
	if _, err = rand.Read(credential); err != nil {
		t.Fatal(err)
	}
	runtimeIdentity := enrollment.RuntimeIdentity{Version: 1, HelperID: "task39_helper_" + username, MachineID: machine, EnvironmentID: "task39_env_" + username, Credential: base64.RawURLEncoding.EncodeToString(credential), ExpiresAt: time.Now().UTC().Add(24 * time.Hour), KeyID: key.ID}
	raw, _ := json.Marshal(runtimeIdentity)
	if err = os.WriteFile(filepath.Join(state, "runtime-identity.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	repository := control + "/windows-amd64/2026.09.08.390"
	base, _ := url.Parse(repository)
	artifact := bootstrap.ArtifactTarget{Schema: bootstrap.ArtifactTargetSchemaV1, Kind: bootstrap.ArtifactKindPB, Version: "2026.09.08.390", Platform: "windows", Architecture: "amd64", RepositoryURL: repository, TargetPath: releaseindex.AssetName("windows", "amd64")}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	executable, err := bootstrap.FetchVerifiedArtifact(ctx, artifact, filepath.Join(state, "tuf"), &http.Client{Transport: task39WindowsTransport{base}, Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := listener.Addr().String()
	_ = listener.Close()
	request := Request{Schema: SchemaV1, Platform: "windows", User: username, OwnerSID: ownerSID, Executable: executable, Artifact: artifact, Home: home, Path: os.Getenv("PATH"), StateRoot: state, WorkspaceRoot: workspace, ControlURL: control, UserMachineID: machine, Shell: "powershell.exe", HelperListenAddress: listen, SetupMode: "host"}
	raw, _ = json.Marshal(request)
	if err = os.WriteFile(filepath.Join(root, "request.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func currentWindowsSID() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func TestTask39RefreshWindowsArtifact(t *testing.T) {
	if *task39WindowsRefreshRequest == "" {
		t.Skip("native owner-context artifact refresh fixture")
	}
	raw, err := os.ReadFile(*task39WindowsRefreshRequest)
	if err != nil {
		t.Fatal(err)
	}
	var request Request
	if err = json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	ownerSID, err := currentWindowsSID()
	if err != nil {
		t.Fatal(err)
	}
	if request.OwnerSID != ownerSID {
		t.Fatal("request owner does not match current Windows user")
	}
	if *task39WindowsRefreshVersion != "" {
		request.Artifact.Version = *task39WindowsRefreshVersion
	}
	base, err := url.Parse(request.Artifact.RepositoryURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		t.Fatal("invalid signed artifact repository")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	executable, err := bootstrap.FetchVerifiedArtifact(ctx, request.Artifact, filepath.Join(request.StateRoot, "tuf"), &http.Client{Transport: task39WindowsTransport{base}, Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	request.Executable = executable
	raw, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(*task39WindowsRefreshRequest, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTask39WindowsLifecycle(t *testing.T) {
	if *task39WindowsAction == "" {
		t.Skip("native privileged lifecycle fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var requests []Request
	for _, path := range strings.Split(*task39WindowsRequests, ",") {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var request Request
		if err = json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
	}
	for _, request := range requests {
		switch *task39WindowsAction {
		case "install":
			if err := Install(ctx, request); err != nil {
				t.Fatalf("install %s: %v", request.User, err)
			}
			if err := Commit(request); err != nil {
				t.Fatalf("commit %s: %v", request.User, err)
			}
		case "uninstall":
			if err := Uninstall(ctx, request); err != nil {
				t.Fatalf("uninstall %s: %v", request.User, err)
			}
		case "purge":
			if err := Purge(ctx, request.OwnerSID); err != nil {
				t.Fatalf("purge %s: %v", request.User, err)
			}
		default:
			t.Fatal("invalid lifecycle action")
		}
	}
}
