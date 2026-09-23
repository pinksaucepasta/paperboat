//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesignature"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseeligibility"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseindex"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

type task39Transport struct {
	base    *url.URL
	corrupt bool
}

func (t task39Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	// The compiled fixture overlay may already have mapped the immutable
	// GitHub URL; corruption targets asset bytes at either transport layer.
	if t.corrupt && strings.Contains(r.URL.Path, "/releases/download/") {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("invalid signed artifact")), Request: r}, nil
	}
	if r.URL.Hostname() != "github.com" {
		return http.DefaultTransport.RoundTrip(r)
	}
	clone := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = t.base.Scheme
	u.Host = t.base.Host
	u.Path = strings.TrimRight(t.base.Path, "/") + r.URL.Path
	clone.URL = &u
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}
func task39HTTP(raw string, corrupt bool) *http.Client {
	base, _ := url.Parse(raw)
	return &http.Client{Transport: task39Transport{base, corrupt}, Timeout: 2 * time.Minute}
}

// Run only as one disposable OS user. Keys are generated on that user's
// machine; no account/private machine key is copied between installations.
func TestTask39NativeUserFixture(t *testing.T) {
	mode := os.Getenv("PAPERBOAT_TASK39_USER_MODE")
	if mode == "" {
		t.Skip("native user child only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if os.Geteuid() == 0 {
		t.Fatal("fixture must run as its ordinary OS user")
	}
	root := filepath.Join(os.Getenv("HOME"), ".paperboat-task39")
	descriptor := filepath.Join(root, "request.json")
	if mode == "check" {
		var request Request
		raw, err := os.ReadFile(descriptor)
		if err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal(raw, &request) != nil {
			t.Fatal("invalid request")
		}
		paths := platformPaths(os.Getuid())
		token, err := os.ReadFile(paths.hostdToken)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(token)
		own, err := hostdproto.NewClient(paths.hostdSocket, token, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		status, err := own.Active(ctx)
		if err != nil || status.State != hostdproto.StateActive {
			t.Fatal("own authenticated runtime is unavailable")
		}
		updaterClient, err := updated.NewClient(paths.updaterSocket, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// launchd reports the process running before updater initialization;
		// like bootstrap, wait for its authenticated application readiness.
		for {
			updaterStatus, statusErr := updaterClient.Status(ctx)
			if statusErr == nil && updaterStatus.Status == "ok" && updaterStatus.Version == request.Artifact.Version {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("own authenticated updater did not become ready: status=%s version=%s error=%v", updaterStatus.Status, updaterStatus.Version, statusErr)
			case <-time.After(100 * time.Millisecond):
			}
		}
		otherUID, err := strconv.Atoi(os.Getenv("PAPERBOAT_TASK39_OTHER_UID"))
		if err != nil {
			t.Fatal(err)
		}
		other := platformPaths(otherUID)
		if _, err = os.ReadFile(other.hostdToken); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("other user's token access: %v", err)
		}
		foreign, err := hostdproto.NewClient(other.hostdSocket, token, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = foreign.Active(ctx); err == nil {
			t.Fatal("another OS user's runtime accepted own token")
		}
		otherUpdater, err := updated.NewClient(other.updaterSocket, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = otherUpdater.Status(ctx); err == nil {
			t.Fatal("another OS user's updater accepted the caller")
		}
		if path := os.Getenv("PAPERBOAT_TASK39_OTHER_KEY"); path != "" {
			if _, err = os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("other user's identity access: %v", err)
			}
		}
		return
	}
	if mode != "prepare" {
		t.Fatal("invalid child mode")
	}
	state, workspace := filepath.Join(root, "state"), filepath.Join(root, "workspace")
	for _, p := range []string{root, state, workspace} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	gid, _ := strconv.Atoi(account.Gid)
	group, err := user.LookupGroupId(account.Gid)
	if err != nil {
		t.Fatal(err)
	}
	store, err := identity.Open(identity.Config{StateRoot: state})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	control := os.Getenv("PAPERBOAT_TASK39_ORIGIN")
	machine := "task39_machine_" + account.Username
	environment := "task39_env_" + account.Username
	if err = store.SaveRegistration(identity.Registration{AccountID: "task39_account_" + account.Username, ServerURL: control, MachineID: machine, EnvironmentID: environment, PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: workspace, InstallationGeneration: 1, SetupMode: "host", SetupRoles: []string{"host"}, UpdatedAt: time.Now().UTC()}); err != nil {
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
	certificate, err := endpointidentity.Sign(private, endpointidentity.Claims{AccountID: "task39_account_" + account.Username, Role: endpointidentity.RoleMachine, EndpointID: machine, NoisePublicKey: endpoint.NoisePublicKey(), QUICPublicKey: endpoint.QUICPublicKey(), Generation: 1, Serial: 1, IssuedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := certificate.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SavePeerEndpointCertificate(public, cert, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// This native installation fixture does not exercise helper token issuance;
	// connected control-plane authorization has a separate production fixture.
	fixtureCredential := make([]byte, 48)
	if _, err = rand.Read(fixtureCredential); err != nil {
		t.Fatal(err)
	}
	defer clear(fixtureCredential)
	runtimeIdentity := enrollment.RuntimeIdentity{Version: 1, HelperID: "task39_helper_" + account.Username, MachineID: machine, EnvironmentID: environment, Credential: base64.RawURLEncoding.EncodeToString(fixtureCredential), ExpiresAt: time.Now().UTC().Add(24 * time.Hour), KeyID: key.ID}
	raw, err := json.Marshal(runtimeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(state, "runtime-identity.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	repository := control + "/" + runtime.GOOS + "-" + runtime.GOARCH + "/2026.09.08.390"
	artifact := bootstrap.ArtifactTarget{Schema: bootstrap.ArtifactTargetSchemaV1, Kind: bootstrap.ArtifactKindPB, Version: "2026.09.08.390", Platform: runtime.GOOS, Architecture: runtime.GOARCH, RepositoryURL: repository, TargetPath: releaseindex.AssetName(runtime.GOOS, runtime.GOARCH)}
	path, err := bootstrap.FetchVerifiedArtifact(ctx, artifact, filepath.Join(state, "tuf"), task39HTTP(repository, false))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" {
		if err = nativesignature.New(nil).Verify(ctx, path, runtime.GOOS, runtime.GOARCH); err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(filepath.Join(state, "extracted"), 0700); err != nil {
			t.Fatal(err)
		}
		path, err = workerupdate.ExtractDarwinPackage(ctx, path, filepath.Join(state, "extracted"))
		if err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := listener.Addr().String()
	listener.Close()
	request := Request{Schema: SchemaV1, Platform: runtime.GOOS, User: account.Username, UID: os.Getuid(), Group: group.Name, GID: gid, Executable: path, Artifact: artifact, Home: account.HomeDir, Path: "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", StateRoot: state, WorkspaceRoot: workspace, ControlURL: control, UserMachineID: machine, Shell: "/bin/bash", HelperListenAddress: listen, SetupMode: "host", InstallationGeneration: 1}
	raw, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(descriptor, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTask39TwoUserNativeInstallUpdateRemove(t *testing.T) {
	names := strings.Split(os.Getenv("PAPERBOAT_TASK39_NATIVE_USERS"), ",")
	if len(names) != 2 {
		t.Skip("two disposable native OS users required")
	}
	if os.Geteuid() != 0 {
		t.Fatal("real privileged native installation required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var requests []Request
	child := func(account *user.User, mode string, other *Request) {
		t.Helper()
		uid, _ := strconv.Atoi(account.Uid)
		gid, _ := strconv.Atoi(account.Gid)
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestTask39NativeUserFixture$", "-test.count=1")
		cmd.Env = []string{"HOME=" + account.HomeDir, "USER=" + account.Username, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "PAPERBOAT_TASK39_USER_MODE=" + mode, "PAPERBOAT_TASK39_ORIGIN=" + os.Getenv("PAPERBOAT_TASK39_ORIGIN")}
		if other != nil {
			cmd.Env = append(cmd.Env, "PAPERBOAT_TASK39_OTHER_UID="+strconv.Itoa(other.UID), "PAPERBOAT_TASK39_OTHER_KEY="+filepath.Join(other.StateRoot, "machine-identity.json"))
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{uint32(gid)}}}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("user %s %s failed: %v %s", account.Username, mode, err, output)
		}
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "pb39") {
			t.Fatal("refusing non-task user")
		}
		account, err := user.Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		child(account, "prepare", nil)
		raw, err := os.ReadFile(filepath.Join(account.HomeDir, ".paperboat-task39/request.json"))
		if err != nil {
			t.Fatal(err)
		}
		var request Request
		if json.Unmarshal(raw, &request) != nil {
			t.Fatal("invalid prepared request")
		}
		requests = append(requests, request)
		t.Setenv("SUDO_UID", strconv.Itoa(request.UID))
		t.Cleanup(func() {
			os.Setenv("SUDO_UID", strconv.Itoa(request.UID))
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cleanupCancel()
			if err := Uninstall(cleanupCtx, request); err != nil {
				t.Error("native cleanup:", err)
			}
		})
		if err = Install(ctx, request); err != nil {
			t.Fatal("native install:", err)
		}
		if err = Commit(request); err != nil {
			t.Fatal(err)
		}
		t.Logf("installed production runtime for uid=%d", request.UID)
	}
	assertProcessOwner := func(request Request) {
		t.Helper()
		output, err := exec.CommandContext(ctx, "ps", "-axo", "uid=,command=").Output()
		if err != nil {
			t.Fatal(err)
		}
		defer clear(output)
		binary := platformPaths(request.UID).worker
		found := false
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[1] != binary || fields[2] != "daemon" || fields[3] != "__runtime-hostd" {
				continue
			}
			if fields[0] != strconv.Itoa(request.UID) {
				t.Fatal("installed hostd process has incorrect OS owner")
			}
			found = true
		}
		if !found {
			t.Fatal("installed hostd process is absent")
		}
	}
	for i, request := range requests {
		assertProcessOwner(request)
		account, _ := user.Lookup(request.User)
		child(account, "check", &requests[1-i])
	}
	t.Log("both installed process owners and cross-user key/socket denials verified")
	a, b := requests[0], requests[1]
	aPaths, bPaths := platformPaths(a.UID), platformPaths(b.UID)
	firstKey, _ := os.ReadFile(filepath.Join(a.StateRoot, "machine-identity.json"))
	secondKey, _ := os.ReadFile(filepath.Join(b.StateRoot, "machine-identity.json"))
	if len(firstKey) == 0 || len(secondKey) == 0 || bytes.Equal(firstKey, secondKey) {
		t.Fatal("OS users did not get distinct private identities")
	}
	clear(firstKey)
	clear(secondKey)
	beforeB, err := os.ReadFile(bPaths.worker)
	if err != nil {
		t.Fatal(err)
	}
	digestB := sha256.Sum256(beforeB)
	clear(beforeB)
	token, err := os.ReadFile(aPaths.hostdToken)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(token)
	client, err := hostdproto.NewClient(aPaths.hostdSocket, token, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	candidateRepo := os.Getenv("PAPERBOAT_TASK39_ORIGIN") + "/" + runtime.GOOS + "-" + runtime.GOARCH + "/2026.09.08.391"
	deferral, err := releaseeligibility.NewFileStore(filepath.Join(aPaths.updateState, "deferral.json"))
	if err != nil {
		t.Fatal(err)
	}
	source := workerupdate.TUFSource{Deferral: deferral, RepositoryURL: candidateRepo, StateRoot: filepath.Join(aPaths.installerState, "task39-tuf"), MachineID: a.UserMachineID, HTTP: task39HTTP(candidateRepo, false), FailureDomain: workerupdate.HostdFailureDomainSource{Client: client, MachineID: a.UserMachineID}}
	initial := workerupdate.TUFSource{RepositoryURL: a.Artifact.RepositoryURL, StateRoot: filepath.Join(aPaths.installerState, "task39-initial"), MachineID: a.UserMachineID, HTTP: task39HTTP(a.Artifact.RepositoryURL, false)}
	active, err := initial.Active(ctx, a.Artifact.Version)
	if err != nil {
		t.Fatal(err)
	}
	candidate, eligible, err := source.ResolveManual(ctx)
	if err != nil || !eligible || candidate.Version != "2026.09.08.391" {
		t.Fatalf("signed candidate selection failed: %v", err)
	}
	bad := source
	bad.StateRoot = filepath.Join(aPaths.installerState, "task39-bad")
	bad.HTTP = task39HTTP(candidateRepo, true)
	if r, e := bad.Fetch(ctx, candidate); e == nil {
		r.Close()
		t.Fatal("wrong signed candidate bytes accepted")
	}
	gate, err := workerupdate.NewDeploymentActivationGate(workerupdate.DeploymentActivationGateConfig{Provider: workerupdate.HostdDeploymentProvider{Client: client}})
	if err != nil {
		t.Fatal(err)
	}
	controller := service.UnixUpdateActivator{Platform: runtime.GOOS, UID: a.UID, Runner: service.ExecRunner{}}
	manager, err := workerupdate.New(workerupdate.Config{ManualActivation: true, StatePath: filepath.Join(aPaths.installerState, "task39-update.json"), Binary: aPaths.worker, BinaryRollback: aPaths.workerRollback, BinaryStaged: aPaths.workerNext, Active: active, OwnerUID: 0, OwnerGID: 0, WorkerUID: a.UID, WorkerGID: a.GID, HostdEndpoint: aPaths.hostdSocket, Capability: token, Fetcher: source, Starter: workerupdate.ExecStarter{}, Hostd: client, Health: updated.HTTPHealth{Endpoint: "http://" + a.HelperListenAddress + "/healthz"}, Gate: gate, MonitorWindow: 2 * time.Second, HealthInterval: time.Second, ActivateRuntime: func(ctx context.Context, version string) (hostdproto.Status, error) {
		if err := controller.RestartHostd(ctx); err != nil {
			return hostdproto.Status{}, err
		}
		for {
			status, err := client.Active(ctx)
			if err == nil && status.WorkerID == "runtime-"+version && status.State == hostdproto.StateActive {
				return status, nil
			}
			select {
			case <-ctx.Done():
				return hostdproto.Status{}, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Activate(ctx, candidate)
	if err != nil || !result.Updated {
		t.Fatalf("native update failed: %v", err)
	}
	afterB, err := os.ReadFile(bPaths.worker)
	if err != nil || sha256.Sum256(afterB) != digestB {
		t.Fatal("updating A changed B binary")
	}
	clear(afterB)
	bAccount, _ := user.Lookup(b.User)
	child(bAccount, "check", &a)
	t.Setenv("SUDO_UID", strconv.Itoa(a.UID))
	if err = Uninstall(ctx, a); err != nil {
		t.Fatal(err)
	}
	bToken, err := os.ReadFile(bPaths.hostdToken)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(bToken)
	bClient, err := hostdproto.NewClient(bPaths.hostdSocket, bToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	status, err := bClient.Active(ctx)
	if err != nil || status.WorkerID != "runtime-"+b.Artifact.Version {
		t.Fatal("removing A stopped or updated B")
	}
	t.Logf("two-user native install/update/remove verified: platform=%s uid_a=%d uid_b=%d unchanged_b_sha256=%s", runtime.GOOS, a.UID, b.UID, hex.EncodeToString(digestB[:]))
}

func TestTask39NativeCleanup(t *testing.T) {
	if os.Getenv("PAPERBOAT_TASK39_CLEANUP") != "1" {
		t.Skip("task native cleanup only")
	}
	if os.Geteuid() != 0 {
		t.Fatal("privileged cleanup required")
	}
	for _, name := range strings.Split(os.Getenv("PAPERBOAT_TASK39_NATIVE_USERS"), ",") {
		if !strings.HasPrefix(name, "pb39") {
			t.Fatal("refusing non-task user")
		}
		account, err := user.Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(account.HomeDir, ".paperboat-task39/request.json"))
		if err != nil {
			t.Fatal(err)
		}
		var request Request
		if json.Unmarshal(raw, &request) != nil {
			t.Fatal("invalid task request")
		}
		t.Setenv("SUDO_UID", strconv.Itoa(request.UID))
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		hostd, updater, inspectErr := pendingInstallers(request, platformPaths(request.UID))
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		for _, installer := range []*service.Installer{hostd, updater} {
			_, statErr := os.Stat(installer.DefinitionPath())
			t.Logf("uid=%d definition=%s stat=%v", request.UID, installer.DefinitionPath(), statErr)
		}
		lifecycle, inspectErr := newLifecycleManager(request, platformPaths(request.UID), nil, hostd, updater)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		_, inspectErr = lifecycle.Inspect(ctx)
		t.Logf("uid=%d pre-uninstall inspect=%v", request.UID, inspectErr)
		if os.Getenv("PAPERBOAT_TASK39_INSPECT_ONLY") == "1" {
			cancel()
			continue
		}
		err = Uninstall(ctx, request)
		cancel()
		if err != nil {
			t.Errorf("uid=%d production uninstall: %v", request.UID, err)
		}
	}
}

func TestTask39ArtifactFaultSurvivesFixtureURLMapping(t *testing.T) {
	for _, raw := range []string{"https://github.com/pinksaucepasta/paperboat-cli/releases/download/2026.09.08.391/pb-darwin-arm64.pkg", "https://coolify.newt-mermaid.ts.net:18439/darwin-arm64/2026.09.08.391/pinksaucepasta/paperboat-cli/releases/download/2026.09.08.391/pb-darwin-arm64.pkg"} {
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := (task39Transport{corrupt: true}).RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(body) != "invalid signed artifact" {
			t.Fatal("asset fault injection did not reach response bytes")
		}
	}
}
