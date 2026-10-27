package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseindex"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// verifyPublished checks the complete metadata set as it will be served. In
// particular, it catches a partial publication that leaves timestamp.json or
// snapshot.json present while omitting the versioned files they reference.
func verifyPublished(repo string) error {
	root, targets, snapshot, timestamp, err := loadSet(repo)
	if err != nil {
		return fmt.Errorf("load published metadata: %w", err)
	}
	if err := verifyRoot(root); err != nil {
		return fmt.Errorf("verify root: %w", err)
	}
	if err := root.VerifyDelegate("targets", targets); err != nil {
		return fmt.Errorf("verify targets: %w", err)
	}
	if err := root.VerifyDelegate("snapshot", snapshot); err != nil {
		return fmt.Errorf("verify snapshot: %w", err)
	}
	if err := root.VerifyDelegate("timestamp", timestamp); err != nil {
		return fmt.Errorf("verify timestamp: %w", err)
	}
	if err := verifyPublishedDeploymentPolicies(targets); err != nil {
		return err
	}
	for _, item := range []struct {
		name string
		body []byte
	}{
		{fmt.Sprintf("%d.root.json", root.Signed.Version), mustMetadataBytes(root)},
		{fmt.Sprintf("%d.targets.json", targets.Signed.Version), mustMetadataBytes(targets)},
		{fmt.Sprintf("%d.snapshot.json", snapshot.Signed.Version), mustMetadataBytes(snapshot)},
	} {
		if err := verifyPublishedFile(filepath.Join(repo, "metadata", item.name), item.body); err != nil {
			return err
		}
	}
	if err := verifyPublishedFile(filepath.Join(repo, "metadata", "timestamp.json"), mustMetadataBytes(timestamp)); err != nil {
		return err
	}
	if meta, ok := timestamp.Signed.Meta["snapshot.json"]; ok {
		if err := verifyMetaReference(repo, fmt.Sprintf("%d.snapshot.json", meta.Version), meta); err != nil {
			return err
		}
	} else {
		return errors.New("timestamp does not reference snapshot.json")
	}
	for name, meta := range snapshot.Signed.Meta {
		if err := verifyMetaReference(repo, fmt.Sprintf("%d.%s", meta.Version, name), meta); err != nil {
			return err
		}
	}
	return nil
}

// decodeAndValidateAssetTargetCustom validates the exact custom metadata
// bytes that will be signed or served. Decoding into the runtime release-index
// type is intentional: it catches omitted required fields at the JSON boundary
// instead of trusting a publisher-side struct that may have silently supplied
// zero values. The same preflight is used before signing and after publication.
func decodeAndValidateAssetTargetCustom(body []byte, now time.Time) (assetTargetCustom, error) {
	if len(body) == 0 {
		return assetTargetCustom{}, errors.New("signed asset custom metadata is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var custom assetTargetCustom
	var extra any
	if err := decoder.Decode(&custom); err != nil {
		return assetTargetCustom{}, fmt.Errorf("decode signed asset custom metadata: %w", err)
	}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return assetTargetCustom{}, errors.New("signed asset custom metadata has trailing JSON")
		}
		return assetTargetCustom{}, fmt.Errorf("decode signed asset custom metadata trailer: %w", err)
	}
	if custom.Schema != "paperboat.tuf-asset/v1" || custom.Kind != "github-release-asset" {
		return assetTargetCustom{}, errors.New("signed asset custom metadata has an invalid schema")
	}
	indexBody, err := json.Marshal(custom.ReleaseIndex)
	if err != nil {
		return assetTargetCustom{}, fmt.Errorf("encode signed release index: %w", err)
	}
	index, err := releaseindex.Decode(bytes.NewReader(indexBody), now)
	if err != nil {
		return assetTargetCustom{}, fmt.Errorf("signed release index is invalid: %w", err)
	}
	target, ok := index.Component("pb")
	if !ok || custom.Version != index.Version || custom.Platform != index.Platform || custom.Architecture != index.Architecture || custom.Format != index.BinaryFormat || custom.AssetName != target.AssetName || custom.Repository != target.Repository || custom.URL != target.DownloadURL || custom.SHA256 != target.SHA256 || custom.Length != target.Length {
		return assetTargetCustom{}, errors.New("signed asset custom metadata does not match its release index")
	}
	return custom, nil
}

func verifyPublishedDeploymentPolicies(targets *metadata.Metadata[metadata.TargetsType]) error {
	return verifyPublishedDeploymentPoliciesAt(targets, time.Now().UTC())
}

func verifyPublishedDeploymentPoliciesAt(targets *metadata.Metadata[metadata.TargetsType], now time.Time) error {
	if targets == nil || len(targets.Signed.Targets) == 0 {
		return errors.New("published TUF targets contain an invalid release set")
	}
	type binding struct{ manifest, plan string }
	versions := make(map[string]binding)
	for name, info := range targets.Signed.Targets {
		// The signed schema validates platform and asset names independently of
		// which products the next release ships.
		if info == nil || info.Custom == nil {
			return fmt.Errorf("published TUF target %s has no signed custom metadata", name)
		}
		custom, err := decodeAndValidateAssetTargetCustom(*info.Custom, now)
		if err != nil {
			return fmt.Errorf("published TUF target %s has invalid deployment policy: %w", name, err)
		}
		target, ok := custom.ReleaseIndex.Component("pb")
		digest, hasDigest := info.Hashes["sha256"]
		if !ok || custom.AssetName != name || target.AssetName != name || !hasDigest || len(digest) != sha256.Size || info.Length != target.Length || hex.EncodeToString(digest) != target.SHA256 {
			return fmt.Errorf("published TUF target %s does not match its signed release index", name)
		}
		plan := custom.ReleaseIndex.DeploymentPlan
		if err := plan.Validate(); err != nil || plan.Version != custom.Version || plan.ManifestSHA256 != custom.ReleaseIndex.ManifestSHA256 || custom.ReleaseIndex.DeploymentPlanSHA256 == "" {
			return fmt.Errorf("published TUF target %s has an invalid deployment policy binding", name)
		}
		actualPlanDigest, err := plan.PlanSHA256()
		if err != nil || actualPlanDigest != custom.ReleaseIndex.DeploymentPlanSHA256 {
			return fmt.Errorf("published TUF target %s has a deployment policy digest mismatch", name)
		}
		identity := binding{custom.ReleaseIndex.ManifestSHA256, custom.ReleaseIndex.DeploymentPlanSHA256}
		if prior, ok := versions[custom.Version]; ok && prior != identity {
			return errors.New("published targets for one version have inconsistent manifest or policy")
		}
		versions[custom.Version] = identity
		if len(custom.ReleaseIndex.ManifestSHA256) != sha256.Size*2 || !lowerHex(custom.ReleaseIndex.ManifestSHA256) {
			return fmt.Errorf("published TUF target %s has an invalid manifest digest", name)
		}

	}
	return nil
}

// validateTargetsForPublication is the write/signing boundary for release
// targets. An empty target set is valid only for a newly initialized
// repository; once targets exist, every selected platform contract must pass
// the same validation used by verify-published.
func validateTargetsForPublication(targets *metadata.Metadata[metadata.TargetsType], now time.Time) error {
	if targets == nil {
		return errors.New("TUF targets metadata is nil")
	}
	if len(targets.Signed.Targets) == 0 {
		return nil
	}
	return verifyPublishedDeploymentPoliciesAt(targets, now)
}

func verifyMetaReference(repo, name string, meta *metadata.MetaFiles) error {
	body, err := os.ReadFile(filepath.Join(repo, "metadata", name))
	if err != nil {
		return fmt.Errorf("missing referenced metadata %s: %w", name, err)
	}
	if meta.Length != 0 && int64(len(body)) != meta.Length {
		return fmt.Errorf("metadata length mismatch %s", name)
	}
	if digest, ok := meta.Hashes["sha256"]; ok {
		sum := sha256.Sum256(body)
		if !bytes.Equal(sum[:], digest) {
			return fmt.Errorf("metadata hash mismatch %s", name)
		}
	}
	return nil
}

func mustMetadataBytes[T metadata.Roles](value *metadata.Metadata[T]) []byte {
	body, err := value.ToBytes(false)
	if err != nil {
		panic(err)
	}
	return body
}

func verifyPublishedFile(path string, expected []byte) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("missing published metadata %s: %w", filepath.Base(path), err)
	}
	if !bytes.Equal(body, expected) {
		return fmt.Errorf("published metadata differs from signed set: %s", filepath.Base(path))
	}
	return nil
}
