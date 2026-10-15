package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseindex"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releasepolicy"
	"github.com/pinksaucepasta/paperboat/internal/selfupdate"
	"github.com/pinksaucepasta/paperboat/tools/releaseplan"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

const windowsNativeQualificationSchema = "paperboat.windows-native-qualification/v1"

var releaseVersionPattern = regexp.MustCompile(`^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$`)

func windowsNativeQualificationTarget(architecture string) string {
	return "windows-" + architecture + "-native-qualification.json"
}

func releaseRepository() string {
	if value := strings.TrimSpace(os.Getenv("PAPERBOAT_GITHUB_REPOSITORY")); value != "" {
		return value
	}
	return "pinksaucepasta/paperboat"
}

func releaseAssetName(platform, architecture string) string {
	return releaseindex.AssetName(platform, architecture)
}

func releaseAssetFormat(platform string) string {
	switch platform {
	case "darwin":
		return "macho"
	case "linux":
		return "elf"
	case "windows":
		return "pe"
	default:
		return ""
	}
}

// Keep the publisher's release-index wire model exactly aligned with the
// runtime verifier. In particular, the policy digests and deployment plan are
// required fields and must never drift into an optional/omitempty encoding.
type componentTarget = releaseindex.Target
type releaseIndex = releaseindex.Index

type windowsNativeQualification struct {
	Schema         string `json:"schema"`
	ReleaseVersion string `json:"release_version"`
	Platform       string `json:"platform"`
	Architecture   string `json:"architecture"`
	Status         string `json:"status"`
	NativeTested   bool   `json:"native_tested"`
	WindowsBuild   string `json:"windows_build"`
	Runner         string `json:"runner"`
}

// assetTargetCustom is the only custom target published for a release. It
// binds the TUF digest to the immutable GitHub asset URL and carries the
// signed release policy inline, so no release-index JSON or target bytes need
// to be served by the Paperboat origin.
type assetTargetCustom struct {
	Schema       string       `json:"schema"`
	Kind         string       `json:"kind"`
	Version      string       `json:"version"`
	Platform     string       `json:"platform"`
	Architecture string       `json:"architecture"`
	Format       string       `json:"format"`
	AssetName    string       `json:"asset_name"`
	Repository   string       `json:"repository"`
	URL          string       `json:"url"`
	SHA256       string       `json:"sha256"`
	Length       int64        `json:"length"`
	ReleaseIndex releaseIndex `json:"release_index"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "paperboat-tuf:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if runtime.GOOS != "darwin" && os.Getenv("PAPERBOAT_TUF_CI") != "1" {
		return errors.New("production signing is restricted to the macOS release workstation")
	}
	if len(args) == 0 {
		return errors.New("usage: paperboat-tuf <init|publish|promote|pause|quarantine|refresh|status|validate-signers|verify-published>")
	}
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		repo := fs.String("repository", "", "repository directory")
		root := fs.String("trusted-root", "", "offline-signed initial root metadata")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New("usage: paperboat-tuf init -repository DIR -trusted-root FILE")
		}
		return initialize(*repo, *root)
	case "publish":
		fs := flag.NewFlagSet("publish", flag.ContinueOnError)
		repo := fs.String("repository", "", "repository directory")
		version := fs.String("version", "", "release version")
		artifacts := fs.String("artifacts", "", "release artifact directory")
		rolloutRevision := fs.Uint64("rollout-revision", 0, "monotonic signed rollout policy revision")
		severity := fs.String("severity", "routine", "routine, security, or critical")
		manifestPath := fs.String("manifest", "", "precomputed exact selected-artifact manifest")
		planPath := fs.String("deployment-plan", "", "precomputed signed deployment policy input")
		supervisorMaintenance := fs.Bool("supervisor-maintenance", false, "release updates stable supervisor components")
		amd64QualificationEvidence := fs.String("windows-amd64-native-evidence", "", "absolute JSON evidence for Windows amd64 native qualification")
		arm64QualificationEvidence := fs.String("windows-arm64-native-evidence", "", "absolute JSON evidence for Windows arm64 native qualification")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New("usage: paperboat-tuf publish -repository DIR -version VERSION -artifacts DIR -manifest FILE -deployment-plan FILE -windows-amd64-native-evidence FILE -windows-arm64-native-evidence FILE -rollout-revision N")
		}
		if *rolloutRevision == 0 || (*severity != "routine" && *severity != "security" && *severity != "critical") {
			return errors.New("valid rollout revision and severity are required")
		}
		return publishWithPolicy(*repo, *version, *artifacts, map[string]string{"amd64": *amd64QualificationEvidence, "arm64": *arm64QualificationEvidence}, *rolloutRevision, *severity, *supervisorMaintenance, *manifestPath, *planPath)
	case "refresh":
		fs := flag.NewFlagSet("refresh", flag.ContinueOnError)
		repo := fs.String("repository", "", "repository directory")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New("usage: paperboat-tuf refresh -repository DIR")
		}
		return refresh(*repo)
	case "promote", "pause", "quarantine":
		fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
		repo := fs.String("repository", "", "repository directory")
		revision := fs.Uint64("rollout-revision", 0, "new monotonic signed rollout policy revision")
		percentage := fs.Uint("percentage", 0, "eligible cohort percentage (promote only)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *revision == 0 || *percentage > 100 || args[0] != "promote" && *percentage != 0 {
			return fmt.Errorf("usage: paperboat-tuf %s -repository DIR -rollout-revision N%s", args[0], map[bool]string{true: " -percentage 0..100"}[args[0] == "promote"])
		}
		return mutateRollout(*repo, args[0], *revision, uint8(*percentage))
	case "status":
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		repo := fs.String("repository", "", "repository directory")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New("usage: paperboat-tuf status -repository DIR")
		}
		return status(*repo)
	case "validate-signers":
		fs := flag.NewFlagSet("validate-signers", flag.ContinueOnError)
		repo := fs.String("repository", "", "repository directory")
		trustedRoot := fs.String("trusted-root", "", "absolute trusted root metadata file")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New("usage: paperboat-tuf validate-signers -repository DIR -trusted-root FILE")
		}
		return validateSigners(*repo, *trustedRoot)
	case "verify-published":
		fs := flag.NewFlagSet("verify-published", flag.ContinueOnError)
		repo := fs.String("repository", "", "repository directory")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
			return errors.New("usage: paperboat-tuf verify-published -repository DIR")
		}
		return verifyPublished(*repo)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// validateSigners performs the release authority gate without changing the
// repository. It first advances from the client's embedded trusted root
// through every numbered root, then binds the configured online private keys
// to the roles authorized by that trusted current root.
func validateSigners(repo, trustedRootPath string) error {
	repo, err := validateRepository(repo, true)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(trustedRootPath) || filepath.Clean(trustedRootPath) != trustedRootPath {
		return errors.New("trusted root path must be absolute and clean")
	}
	trusted, err := metadata.Root().FromFile(trustedRootPath)
	if err != nil {
		return fmt.Errorf("load trusted root: %w", err)
	}
	if err := verifyRoot(trusted); err != nil {
		return fmt.Errorf("verify trusted root: %w", err)
	}
	current := trusted
	for version := trusted.Signed.Version + 1; ; version++ {
		path := filepath.Join(repo, "metadata", fmt.Sprintf("%d.root.json", version))
		next, loadErr := metadata.Root().FromFile(path)
		if errors.Is(loadErr, os.ErrNotExist) {
			break
		}
		if loadErr != nil {
			return fmt.Errorf("load root version %d: %w", version, loadErr)
		}
		if next.Signed.Version != version {
			return fmt.Errorf("root version %d has invalid signed version %d", version, next.Signed.Version)
		}
		if err := current.VerifyDelegate("root", next); err != nil {
			return fmt.Errorf("verify root version %d with previous root: %w", version, err)
		}
		if err := verifyRoot(next); err != nil {
			return fmt.Errorf("verify root version %d with new root: %w", version, err)
		}
		current = next
	}
	served, err := metadata.Root().FromFile(filepath.Join(repo, "metadata", "root.json"))
	if err != nil {
		return fmt.Errorf("load served root: %w", err)
	}
	if served.Signed.Version != current.Signed.Version || !bytes.Equal(mustMetadataBytes(served), mustMetadataBytes(current)) {
		return errors.New("served root does not match the trusted numbered root chain")
	}
	err = validateOnlineSigner(current)
	if err != nil {
		return err
	}
	fmt.Printf("root=%d online_signers=1 validated\n", current.Signed.Version)
	return nil
}

func publish(repo, version, artifacts string, qualificationEvidencePaths map[string]string, rolloutRevision uint64, severity string, supervisorMaintenance bool) error {
	return publishWithPolicy(repo, version, artifacts, qualificationEvidencePaths, rolloutRevision, severity, supervisorMaintenance, "", "")
}

func publishWithPolicy(repo, version, artifacts string, qualificationEvidencePaths map[string]string, rolloutRevision uint64, severity string, supervisorMaintenance bool, manifestPath, planPath string) error {
	repo, err := validateRepository(repo, true)
	if err != nil {
		return err
	}
	if !releaseVersionPattern.MatchString(version) || !filepath.IsAbs(artifacts) || filepath.Clean(artifacts) != artifacts {
		return errors.New("version must be YYYY.MM.DD.N and artifacts must be an absolute clean directory")
	}
	manifest, deploymentPlan, err := loadPublicationPolicy(artifacts, version, rolloutRevision, severity, manifestPath, planPath)
	if err != nil {
		return err
	}
	manifestSHA256, err := manifest.SHA256()
	if err != nil {
		return fmt.Errorf("hash release manifest: %w", err)
	}
	deploymentPlanSHA256, err := deploymentPlan.PlanSHA256()
	if err != nil {
		return fmt.Errorf("hash deployment plan: %w", err)
	}
	qualifications := make(map[string]windowsNativeQualification, 2)
	for _, asset := range manifest.Artifacts {
		if asset.Platform != "windows" {
			continue
		}
		architecture := asset.Architecture
		path := strings.TrimSpace(qualificationEvidencePaths[architecture])
		if path == "" {
			return fmt.Errorf("absolute Windows %s native qualification evidence is required", architecture)
		}
		qualification, _, _, err := loadWindowsNativeQualification(path, architecture)
		if err != nil {
			return err
		}
		if err := validateWindowsNativeQualificationHeader(qualification, version, architecture); err != nil {
			return err
		}
		qualifications[architecture] = qualification
	}
	root, targets, snapshot, timestamp, err := loadSet(repo)
	if err != nil {
		return err
	}
	if err := verifyRoot(root); err != nil {
		return err
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		return fmt.Errorf("verify existing targets: %w", err)
	}
	if err := root.VerifyDelegate("snapshot", snapshot); err != nil {
		return err
	}
	if err := root.VerifyDelegate("timestamp", timestamp); err != nil {
		return err
	}
	if err := validateTargetsForPublication(targets, time.Now().UTC()); err != nil {
		return err
	}
	err = validateOnlineSigner(root)
	if err != nil {
		return err
	}
	createdAt := time.Now().UTC()
	githubRepository := releaseRepository()
	for _, asset := range manifest.Artifacts {
		platform, architecture := asset.Platform, asset.Architecture
		format := releaseAssetFormat(platform)
		name := releaseAssetName(platform, architecture)
		local := filepath.Join(artifacts, name)
		info, err := metadata.TargetFile().FromFile(local, "sha256")
		if err != nil {
			return fmt.Errorf("release asset %s: %w", name, err)
		}
		digest := hex.EncodeToString(info.Hashes["sha256"])
		manifestArtifact, ok := manifest.Artifact(name)
		if !ok || manifestArtifact.Length != info.Length || manifestArtifact.SHA256 != digest {
			return fmt.Errorf("release asset %s does not match the release manifest", name)
		}
		downloadURL := "https://github.com/" + githubRepository + "/releases/download/" + version + "/" + name
		if !releaseindex.ValidDownloadURL(downloadURL, githubRepository, version, name) {
			return fmt.Errorf("GitHub repository %q or release URL is invalid", githubRepository)
		}
		var previousIndex releaseIndex
		if old := targets.Signed.Targets[name]; old != nil {
			previous, err := decodeAndValidateAssetTargetCustom(*old.Custom, createdAt)
			if err != nil {
				return err
			}
			comparison, err := selfupdate.CompareVersions(version, previous.Version)
			if err != nil || comparison <= 0 {
				return fmt.Errorf("release asset %s must advance its published version", name)
			}
			previousIndex = previous.ReleaseIndex
		}
		component := componentTarget{Component: "pb", TargetPath: name, AssetName: name, Repository: githubRepository, DownloadURL: downloadURL, SHA256: digest, Length: info.Length, Platform: platform, Architecture: architecture, BinaryFormat: format}
		channel, stability, nativeTested := "stable", "", false
		var testedBuilds []string
		openSSHID, openSSHVersion := "", ""
		if platform == "windows" {
			stability = "stable"
			openSSHID, openSSHVersion = "Microsoft.OpenSSH.Preview", "10.0.0.0"
			qualification := qualifications[architecture]
			nativeTested, testedBuilds = true, []string{qualification.WindowsBuild}
		}
		planCopy := deploymentPlan
		index := releaseIndex{Schema: "paperboat.release-index/v1", ReleaseID: "rel_" + version, Version: version, Channel: channel, Severity: severity, CreatedAt: createdAt, Platform: platform, Architecture: architecture, BinaryFormat: format, Targets: []componentTarget{component}, HostdAPIMin: 1, HostdAPIMax: 2, RuntimeAPIMin: 1, RuntimeAPIMax: 2, RolloutPolicyRevision: deploymentPlan.PolicyRevision, SupervisorMaintenance: supervisorMaintenance, Stability: stability, NativeTested: nativeTested, TestedWindowsBuilds: testedBuilds, OpenSSHPackageID: openSSHID, OpenSSHApprovedVersion: openSSHVersion, ManifestSHA256: manifestSHA256, DeploymentPlanSHA256: deploymentPlanSHA256, DeploymentPlan: &planCopy}
		index.MinimumVersion = previousIndex.MinimumVersion
		index.RevokedVersions = append([]string(nil), previousIndex.RevokedVersions...)
		custom := assetTargetCustom{Schema: "paperboat.tuf-asset/v1", Kind: "github-release-asset", Version: version, Platform: platform, Architecture: architecture, Format: format, AssetName: name, Repository: githubRepository, URL: downloadURL, SHA256: digest, Length: info.Length, ReleaseIndex: index}
		customBody, err := json.Marshal(custom)
		if err != nil {
			return err
		}
		if _, err := decodeAndValidateAssetTargetCustom(customBody, createdAt); err != nil {
			return fmt.Errorf("release asset %s failed publication preflight: %w", name, err)
		}
		raw := json.RawMessage(customBody)
		info.Custom, info.Path = &raw, name
		targets.Signed.Targets[name] = info
	}
	return signTargetsSet(repo, root, targets, snapshot, timestamp)
}

func loadPublicationPolicy(artifacts, version string, rolloutRevision uint64, severity, manifestPath, planPath string) (releaseplan.Manifest, releaseplan.Plan, error) {
	if (manifestPath == "") != (planPath == "") {
		return releaseplan.Manifest{}, releaseplan.Plan{}, errors.New("manifest and deployment plan must be supplied together")
	}
	if manifestPath != "" {
		manifest, err := releaseplan.LoadManifest(manifestPath)
		if err != nil {
			return releaseplan.Manifest{}, releaseplan.Plan{}, fmt.Errorf("load release manifest: %w", err)
		}
		if err := releaseplan.VerifyManifest(manifest, artifacts); err != nil {
			return releaseplan.Manifest{}, releaseplan.Plan{}, fmt.Errorf("verify release manifest: %w", err)
		}
		plan, err := releaseplan.LoadPlan(planPath)
		if err != nil {
			return releaseplan.Manifest{}, releaseplan.Plan{}, fmt.Errorf("load deployment plan: %w", err)
		}
		if err := releaseplan.ValidatePlanAgainstManifest(plan, manifest); err != nil || plan.Version != version || plan.PolicyRevision != rolloutRevision || plan.Severity != severity {
			return releaseplan.Manifest{}, releaseplan.Plan{}, errors.New("deployment plan does not match release publication inputs")
		}
		return manifest, plan, nil
	}
	commit, err := releaseSourceCommit()
	if err != nil {
		return releaseplan.Manifest{}, releaseplan.Plan{}, err
	}
	toolchain, err := releaseToolchain()
	if err != nil {
		return releaseplan.Manifest{}, releaseplan.Plan{}, err
	}
	entries, err := os.ReadDir(artifacts)
	if err != nil {
		return releaseplan.Manifest{}, releaseplan.Plan{}, err
	}
	for _, entry := range entries {
		known := false
		for _, spec := range releaseplan.ArtifactSpecs() {
			if entry.Name() == spec.Name {
				known = true
				break
			}
		}
		if entry.Name() == windowsNativeQualificationTarget("amd64") || entry.Name() == windowsNativeQualificationTarget("arm64") {
			known = true
		}
		if !known || !entry.Type().IsRegular() {
			return releaseplan.Manifest{}, releaseplan.Plan{}, errors.New("artifact directory contains unsupported entries")
		}
	}
	specs := releaseplan.ArtifactSpecs()
	artifactsForManifest := make([]releaseplan.Artifact, 0, len(specs))
	for _, spec := range specs {
		path := filepath.Join(artifacts, spec.Name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > releaseplan.MaxArtifactBytes {
			return releaseplan.Manifest{}, releaseplan.Plan{}, fmt.Errorf("release asset %s is invalid", spec.Name)
		}
		file, err := os.Open(path)
		if err != nil {
			return releaseplan.Manifest{}, releaseplan.Plan{}, fmt.Errorf("open release asset %s: %w", spec.Name, err)
		}
		hash := sha256.New()
		length, copyErr := io.Copy(hash, io.LimitReader(file, releaseplan.MaxArtifactBytes+1))
		stat, statErr := file.Stat()
		closeErr := file.Close()
		if copyErr != nil || statErr != nil || closeErr != nil || stat == nil || length != info.Size() || length > releaseplan.MaxArtifactBytes || !os.SameFile(info, stat) || !info.ModTime().Equal(stat.ModTime()) {
			return releaseplan.Manifest{}, releaseplan.Plan{}, fmt.Errorf("release asset %s changed while hashing", spec.Name)
		}
		artifactsForManifest = append(artifactsForManifest, releaseplan.Artifact{Name: spec.Name, Platform: spec.Platform, Architecture: spec.Architecture, Format: spec.Format, Length: length, SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	manifest, err := releaseplan.NewManifest(version, commit, toolchain, artifactsForManifest)
	if err != nil {
		return releaseplan.Manifest{}, releaseplan.Plan{}, fmt.Errorf("build release manifest: %w", err)
	}
	plan, err := releaseplan.DefaultPlan(manifest, rolloutRevision, severity, "release-"+version)
	if err != nil {
		return releaseplan.Manifest{}, releaseplan.Plan{}, fmt.Errorf("build deployment plan: %w", err)
	}
	return manifest, plan, nil
}

func releaseSourceCommit() (string, error) {
	for _, name := range []string{"PAPERBOAT_RELEASE_SOURCE_COMMIT", "GITHUB_SHA"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			if !regexp.MustCompile(`^[a-f0-9]{40,64}$`).MatchString(value) {
				return "", fmt.Errorf("%s is not a lowercase commit SHA", name)
			}
			return value, nil
		}
	}
	output, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", errors.New("release source commit is unavailable; set PAPERBOAT_RELEASE_SOURCE_COMMIT")
	}
	value := strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^[a-f0-9]{40,64}$`).MatchString(value) {
		return "", errors.New("git release source commit is invalid")
	}
	return value, nil
}

func releaseToolchain() (string, error) {
	value := strings.TrimSpace(os.Getenv("PAPERBOAT_RELEASE_TOOLCHAIN"))
	if value == "" {
		value = runtime.Version()
	}
	if !regexp.MustCompile(`^go[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(value) {
		return "", errors.New("release toolchain is unavailable; set PAPERBOAT_RELEASE_TOOLCHAIN")
	}
	return value, nil
}

func lowerHex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) > 0 && value == strings.ToLower(value)
}

func loadWindowsNativeQualification(path, architecture string) (windowsNativeQualification, []byte, string, error) {
	if architecture != "amd64" && architecture != "arm64" {
		return windowsNativeQualification{}, nil, "", errors.New("Windows native qualification architecture is invalid")
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return windowsNativeQualification{}, nil, "", fmt.Errorf("absolute Windows %s native qualification evidence is required", architecture)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > 1<<20 {
		return windowsNativeQualification{}, nil, "", fmt.Errorf("Windows %s native qualification evidence file is invalid", architecture)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return windowsNativeQualification{}, nil, "", fmt.Errorf("read Windows %s native qualification evidence: %w", architecture, err)
	}
	var qualification windowsNativeQualification
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&qualification) != nil || decoder.Decode(&extra) != io.EOF {
		return windowsNativeQualification{}, nil, "", fmt.Errorf("Windows %s native qualification evidence is malformed", architecture)
	}
	digest := sha256.Sum256(body)
	return qualification, body, hex.EncodeToString(digest[:]), nil
}

func validateWindowsNativeQualificationHeader(qualification windowsNativeQualification, version, architecture string) error {
	if qualification.Schema != windowsNativeQualificationSchema || qualification.ReleaseVersion != version || qualification.Platform != "windows" || qualification.Architecture != architecture || qualification.Status != "passed" || !qualification.NativeTested || !safeEvidenceValue(qualification.WindowsBuild) || !safeEvidenceValue(qualification.Runner) {
		return fmt.Errorf("Windows %s native qualification evidence is incomplete or not passed", architecture)
	}
	return nil
}

func safeEvidenceValue(value string) bool {
	return len(value) >= 1 && len(value) <= 128 && !strings.ContainsAny(value, "\x00\r\n")
}

func refresh(repo string) error {
	repo, err := validateRepository(repo, true)
	if err != nil {
		return err
	}
	root, targets, snapshot, timestamp, err := loadSet(repo)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := validateRefreshMetadataFreshness(root, targets, now); err != nil {
		return err
	}
	if err := validateTargetsForPublication(targets, now); err != nil {
		return fmt.Errorf("release publication preflight failed: %w", err)
	}
	err = validateOnlineSigner(root)
	if err != nil {
		return err
	}
	snapshot.Signed.Version++
	snapshot.Signed.Expires = now.Add(7 * 24 * time.Hour)
	snapshot.Signed.Meta["targets.json"] = metadata.MetaFile(targets.Signed.Version)
	snapshot.ClearSignatures()
	if err := sign(snapshot); err != nil {
		return err
	}
	timestamp.Signed.Version++
	timestamp.Signed.Expires = now.Add(24 * time.Hour)
	timestamp.Signed.Meta["snapshot.json"] = metadata.MetaFile(snapshot.Signed.Version)
	timestamp.ClearSignatures()
	if err := sign(timestamp); err != nil {
		return err
	}
	return writeSet(repo, root, targets, snapshot, timestamp)
}

func validateRefreshMetadataFreshness(root *metadata.Metadata[metadata.RootType], targets *metadata.Metadata[metadata.TargetsType], now time.Time) error {
	if root == nil || !root.Signed.Expires.After(now) {
		if root == nil {
			return errors.New("cannot refresh metadata with a missing root expiration")
		}
		return fmt.Errorf("cannot refresh metadata with expired root metadata: expires %s", root.Signed.Expires.UTC().Format(time.RFC3339Nano))
	}
	if targets == nil || !targets.Signed.Expires.After(now) {
		if targets == nil {
			return errors.New("cannot refresh metadata with a missing targets expiration")
		}
		return fmt.Errorf("cannot refresh metadata with expired targets metadata: expires %s", targets.Signed.Expires.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// mutateRollout changes only the signed static deployment policy carried by
// every release target. It never accepts artifact paths or target names from
// the caller. Each published platform retains its release/manifest binding and
// receives the requested monotonic revision before metadata is re-signed.
func mutateRollout(repo, operation string, revision uint64, percentage uint8) error {
	if revision == 0 || operation == "promote" && (percentage == 0 || percentage > 100) || operation != "promote" && percentage != 0 {
		return errors.New("invalid deployment policy mutation")
	}
	if operation != "promote" && operation != "pause" && operation != "quarantine" {
		return errors.New("invalid deployment policy mutation")
	}
	repo, err := validateRepository(repo, true)
	if err != nil {
		return err
	}
	root, targets, snapshot, timestamp, err := loadSet(repo)
	if err != nil {
		return err
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		return err
	}
	if err := verifyPublishedDeploymentPolicies(targets); err != nil {
		return err
	}
	err = validateOnlineSigner(root)
	if err != nil {
		return err
	}
	type update struct {
		info *metadata.TargetFiles
		name string
		raw  json.RawMessage
	}
	updates := make([]update, 0, len(supportedReleaseTargets()))
	for name, info := range targets.Signed.Targets {
		if info == nil || len(info.Hashes["sha256"]) != sha256.Size || info.Custom == nil {
			return fmt.Errorf("signed release asset %s is unavailable", name)
		}
		var custom assetTargetCustom
		decoder := json.NewDecoder(strings.NewReader(string(*info.Custom)))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&custom) != nil || decoder.Decode(&extra) != io.EOF || custom.Schema != "paperboat.tuf-asset/v1" || custom.Kind != "github-release-asset" || custom.ReleaseIndex.Schema != "paperboat.release-index/v1" || custom.ReleaseIndex.DeploymentPlan == nil {
			return fmt.Errorf("signed release asset %s cannot accept revision %d", name, revision)
		}
		plan := *custom.ReleaseIndex.DeploymentPlan
		if plan.ValidateAgainst(custom.ReleaseIndex.Version, custom.ReleaseIndex.ManifestSHA256) != nil || custom.ReleaseIndex.Version != custom.Version || custom.ReleaseIndex.ManifestSHA256 == "" {
			return fmt.Errorf("signed release asset %s has an invalid deployment policy binding", name)
		}
		currentPlanDigest, err := plan.PlanSHA256()
		if err != nil || currentPlanDigest != custom.ReleaseIndex.DeploymentPlanSHA256 || custom.ReleaseIndex.RolloutPolicyRevision != plan.PolicyRevision {
			return fmt.Errorf("signed release asset %s has inconsistent deployment policy metadata", name)
		}
		if err := applyDeploymentMutation(&plan, operation, revision, percentage); err != nil {
			return fmt.Errorf("mutate deployment policy %s: %w", name, err)
		}
		updatedPlanDigest, err := plan.PlanSHA256()
		if err != nil {
			return fmt.Errorf("hash deployment policy %s: %w", name, err)
		}
		custom.ReleaseIndex.RolloutPolicyRevision = plan.PolicyRevision
		custom.ReleaseIndex.DeploymentPlanSHA256 = updatedPlanDigest
		custom.ReleaseIndex.DeploymentPlan = &plan
		updated, err := json.Marshal(custom)
		if err != nil {
			return err
		}
		updates = append(updates, update{info: info, name: name, raw: json.RawMessage(updated)})
	}
	for _, item := range updates {
		raw := item.raw
		item.info.Custom, item.info.Path = &raw, item.name
	}
	return signTargetsSet(repo, root, targets, snapshot, timestamp)
}

type releaseTargetPlatform struct {
	platform     string
	architecture string
}

func supportedReleaseTargets() []releaseTargetPlatform {
	specs := releaseplan.ArtifactSpecs()
	targets := make([]releaseTargetPlatform, 0, len(specs))
	for _, spec := range specs {
		targets = append(targets, releaseTargetPlatform{platform: spec.Platform, architecture: spec.Architecture})
	}
	return targets
}

func applyDeploymentMutation(plan *releasepolicy.Plan, operation string, revision uint64, percentage uint8) error {
	if plan == nil || plan.Validate() != nil || revision == 0 || revision <= plan.PolicyRevision {
		return errors.New("deployment policy mutation is not monotonic")
	}
	switch operation {
	case "promote":
		if percentage == 0 || percentage > 100 || plan.RolloutState == releasepolicy.RolloutStateQuarantined {
			return errors.New("invalid promote policy")
		}
		plan.RolloutState = releasepolicy.RolloutStateActive
		for index := range plan.Cohorts {
			if plan.Cohorts[index].Name != "general" && plan.Cohorts[index].Percentage < percentage {
				plan.Cohorts[index].Percentage = percentage
			}
		}
	case "pause":
		if percentage != 0 || plan.RolloutState == releasepolicy.RolloutStateQuarantined {
			return errors.New("invalid pause policy")
		}
		plan.RolloutState = releasepolicy.RolloutStatePaused
	case "quarantine":
		if percentage != 0 {
			return errors.New("invalid quarantine policy")
		}
		plan.RolloutState = releasepolicy.RolloutStateQuarantined
	default:
		return errors.New("invalid deployment policy mutation")
	}
	plan.PolicyRevision = revision
	if err := plan.Validate(); err != nil {
		return err
	}
	return nil
}

func signTargetsSet(repo string, root *metadata.Metadata[metadata.RootType], targets *metadata.Metadata[metadata.TargetsType], snapshot *metadata.Metadata[metadata.SnapshotType], timestamp *metadata.Metadata[metadata.TimestampType]) error {
	now := time.Now().UTC()
	if err := validateTargetsForPublication(targets, now); err != nil {
		return fmt.Errorf("release publication preflight failed: %w", err)
	}
	targets.Signed.Version++
	targets.Signed.Expires = now.Add(90 * 24 * time.Hour)
	targets.ClearSignatures()
	if err := sign(targets); err != nil {
		return err
	}
	snapshot.Signed.Version++
	snapshot.Signed.Expires = now.Add(7 * 24 * time.Hour)
	snapshot.Signed.Meta["targets.json"] = metadata.MetaFile(targets.Signed.Version)
	snapshot.ClearSignatures()
	if err := sign(snapshot); err != nil {
		return err
	}
	timestamp.Signed.Version++
	timestamp.Signed.Expires = now.Add(24 * time.Hour)
	timestamp.Signed.Meta["snapshot.json"] = metadata.MetaFile(snapshot.Signed.Version)
	timestamp.ClearSignatures()
	if err := sign(timestamp); err != nil {
		return err
	}
	return writeSet(repo, root, targets, snapshot, timestamp)
}

func status(repo string) error {
	repo, err := validateRepository(repo, true)
	if err != nil {
		return err
	}
	root, targets, snapshot, timestamp, err := loadSet(repo)
	if err != nil {
		return err
	}
	if err := verifyRoot(root); err != nil {
		return err
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		return err
	}
	if err := root.VerifyDelegate("snapshot", snapshot); err != nil {
		return err
	}
	if err := root.VerifyDelegate("timestamp", timestamp); err != nil {
		return err
	}
	fmt.Printf("root=%d expires=%s targets=%d expires=%s snapshot=%d expires=%s timestamp=%d expires=%s targets_count=%d\n",
		root.Signed.Version, root.Signed.Expires.Format(time.RFC3339), targets.Signed.Version, targets.Signed.Expires.Format(time.RFC3339), snapshot.Signed.Version, snapshot.Signed.Expires.Format(time.RFC3339), timestamp.Signed.Version, timestamp.Signed.Expires.Format(time.RFC3339), len(targets.Signed.Targets))
	return nil
}

func validateRepository(path string, existing bool) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("repository path must be absolute and clean")
	}
	if existing {
		if info, err := os.Lstat(path); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("repository directory is invalid")
		}
	}
	return path, nil
}

func loadSet(repo string) (*metadata.Metadata[metadata.RootType], *metadata.Metadata[metadata.TargetsType], *metadata.Metadata[metadata.SnapshotType], *metadata.Metadata[metadata.TimestampType], error) {
	root, err := metadata.Root().FromFile(filepath.Join(repo, "metadata", "root.json"))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	targets, err := metadata.Targets().FromFile(filepath.Join(repo, "metadata", "targets.json"))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	snapshot, err := metadata.Snapshot().FromFile(filepath.Join(repo, "metadata", "snapshot.json"))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	timestamp, err := metadata.Timestamp().FromFile(filepath.Join(repo, "metadata", "timestamp.json"))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return root, targets, snapshot, timestamp, nil
}

func writeSet(repo string, root *metadata.Metadata[metadata.RootType], targets *metadata.Metadata[metadata.TargetsType], snapshot *metadata.Metadata[metadata.SnapshotType], timestamp *metadata.Metadata[metadata.TimestampType]) error {
	if err := os.MkdirAll(filepath.Join(repo, "metadata"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(repo, "targets"), 0o755); err != nil {
		return err
	}
	if err := validateTargetsForPublication(targets, time.Now().UTC()); err != nil {
		return fmt.Errorf("release publication preflight failed: %w", err)
	}
	if err := verifyRoot(root); err != nil {
		return err
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		return err
	}
	if err := root.VerifyDelegate("snapshot", snapshot); err != nil {
		return err
	}
	if err := root.VerifyDelegate("timestamp", timestamp); err != nil {
		return err
	}
	entries := []struct {
		name string
		body []byte
	}{}
	for _, item := range []struct {
		name   string
		encode func(bool) ([]byte, error)
	}{
		{fmt.Sprintf("%d.root.json", root.Signed.Version), root.ToBytes}, {"root.json", root.ToBytes},
		{fmt.Sprintf("%d.targets.json", targets.Signed.Version), targets.ToBytes}, {"targets.json", targets.ToBytes},
		{fmt.Sprintf("%d.snapshot.json", snapshot.Signed.Version), snapshot.ToBytes}, {"snapshot.json", snapshot.ToBytes}, {"timestamp.json", timestamp.ToBytes},
	} {
		body, err := item.encode(false)
		if err != nil {
			return err
		}
		entries = append(entries, struct {
			name string
			body []byte
		}{item.name, body})
	}
	for _, entry := range entries {
		if err := atomicWrite(filepath.Join(repo, "metadata", entry.name), entry.body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func verifyRoot(root *metadata.Metadata[metadata.RootType]) error {
	return root.VerifyDelegate("root", root)
}

type atomicWriteFailure struct {
	message string
	cause   error
}

func (failure atomicWriteFailure) Error() string { return failure.message }
func (failure atomicWriteFailure) Unwrap() error { return failure.cause }

func atomicWrite(path string, body []byte, mode os.FileMode) (resultErr error) {
	//paperboat:allow-source-policy atomic-replacement owner=tuf-repository reason=same-directory-fsynced-staging
	tmp, err := os.CreateTemp(filepath.Dir(path), ".paperboat-tuf-")
	if err != nil {
		return atomicWriteFailure{message: "create TUF metadata staging file failed", cause: err}
	}
	name := tmp.Name()
	open := true
	ok := false
	defer func() {
		if open {
			if err := tmp.Close(); err != nil {
				resultErr = errors.Join(resultErr, atomicWriteFailure{message: "close TUF metadata staging file after failure failed", cause: err})
			}
		}
		if !ok {
			if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, atomicWriteFailure{message: "remove TUF metadata staging file after failure failed", cause: err})
			}
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return atomicWriteFailure{message: "set TUF metadata staging file permissions failed", cause: err}
	}
	if _, err := tmp.Write(body); err != nil {
		return atomicWriteFailure{message: "write TUF metadata staging file failed", cause: err}
	}
	if err := tmp.Sync(); err != nil {
		return atomicWriteFailure{message: "sync TUF metadata staging file failed", cause: err}
	}
	if err := tmp.Close(); err != nil {
		open = false
		return atomicWriteFailure{message: "close TUF metadata staging file failed", cause: err}
	}
	open = false
	//paperboat:allow-source-policy atomic-replacement owner=tuf-repository reason=verified-metadata-publication
	if err := replaceFile(name, path); err != nil {
		return atomicWriteFailure{message: "replace TUF repository metadata failed", cause: err}
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return atomicWriteFailure{message: "sync TUF repository metadata directory failed", cause: err}
	}
	ok = true
	return nil
}
