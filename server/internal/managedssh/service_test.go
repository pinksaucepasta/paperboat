package managedssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"

	"golang.org/x/crypto/ssh"
)

type fakeRepository struct {
	host    HostKeySet
	target  MachineTarget
	cas     []UserCA
	osUser  string
	revoked []string
	err     error
}

func (r *fakeRepository) ensureCA(candidate NewUserCA) {
	if len(r.cas) == 0 {
		r.cas = append(r.cas, UserCA{ID: candidate.ID, State: "active", PublicKey: candidate.PublicKey, PrivateKeyCiphertext: candidate.PrivateKeyCiphertext})
	}
}
func (r *fakeRepository) CertificateAuthority(_ context.Context, _ IssueCertificateRequest, candidate NewUserCA) (CertificateAuthorization, error) {
	if r.err != nil {
		return CertificateAuthorization{}, r.err
	}
	r.ensureCA(candidate)
	for _, ca := range r.cas {
		if ca.State == "active" {
			return CertificateAuthorization{OSUser: r.osUser, CA: ca}, nil
		}
	}
	return CertificateAuthorization{}, ErrUnavailable
}
func (r *fakeRepository) UserCAs(_ context.Context, _ UserCAsRequest, candidate NewUserCA) ([]UserCA, []string, error) {
	r.ensureCA(candidate)
	return r.cas, r.revoked, r.err
}
func (r *fakeRepository) RotateUserCA(_ context.Context, _ RotateUserCARequest, candidate NewUserCA) ([]UserCA, error) {
	r.cas = append(r.cas, UserCA{ID: candidate.ID, State: "pending", PublicKey: candidate.PublicKey, PrivateKeyCiphertext: candidate.PrivateKeyCiphertext})
	return r.cas, r.err
}
func (r *fakeRepository) ObserveHost(_ context.Context, _ ObserveHostRequest, set HostKeySet) (HostKeySet, error) {
	r.host = set
	return set, r.err
}
func (r *fakeRepository) PromoteHost(_ context.Context, request PromoteHostRequest) (HostKeySet, error) {
	result := r.host
	result.State, result.PromotedAt = "active", request.Now
	return result, r.err
}
func (r *fakeRepository) GetActiveHost(_ context.Context, _ GetHostKeySetRequest) (HostKeySet, error) {
	return r.host, r.err
}
func (r *fakeRepository) GetPendingHost(_ context.Context, _ GetHostKeySetRequest) (HostKeySet, error) {
	return r.host, r.err
}
func (r *fakeRepository) RegisterTarget(_ context.Context, request RegisterTargetRequest) (MachineTarget, error) {
	r.target = MachineTarget{UserMachineID: request.UserMachineID, MachineGeneration: request.MachineGeneration, OSUser: request.OSUser, TargetPort: request.TargetPort, ReconciliationVersion: 1, CreatedAt: request.Now, UpdatedAt: request.Now}
	return r.target, r.err
}
func (r *fakeRepository) UpdateTargetPort(_ context.Context, request UpdateTargetPortRequest) (MachineTarget, error) {
	r.target.TargetPort, r.target.ReconciliationVersion, r.target.UpdatedAt = request.TargetPort, request.ExpectedReconciliationVersion+1, request.Now
	return r.target, r.err
}
func (r *fakeRepository) GetTarget(_ context.Context, _ GetTargetRequest) (MachineTarget, error) {
	return r.target, r.err
}

func TestServiceHostSetFingerprintIsOrderIndependentAndRejectsDuplicates(t *testing.T) {
	repository := &fakeRepository{}
	service, _ := NewService(repository, testCertificates)
	now := time.Unix(1_800_000_000, 0).UTC()
	first, second := publicLine(t, "ed25519"), publicLine(t, "rsa")
	request := ObserveHostRequest{OperationID: "operation_managed_ssh_01", UserID: "user_01", UserMachineID: "machine_01", MachineGeneration: 2, ObservationGeneration: 3, PublicKeys: []string{first, second}, Now: now}
	left, err := service.ObserveHost(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	suffix, ok := strings.CutPrefix(left.ID, "keyset_")
	id, parseErr := uuid.Parse(suffix)
	if !ok || parseErr != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != suffix {
		t.Fatalf("invalid generated set ID %q", left.ID)
	}
	request.ObservationGeneration++
	request.PublicKeys = []string{second, first}
	right, err := service.ObserveHost(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if left.Fingerprint != right.Fingerprint || len(left.Keys) != 2 || left.Keys[0].Fingerprint == left.Keys[1].Fingerprint {
		t.Fatalf("left=%+v right=%+v", left, right)
	}
	request.PublicKeys = []string{first, first}
	if _, err := service.ObserveHost(context.Background(), request); !errors.Is(err, wire.ErrInvalid) {
		t.Fatalf("duplicate error=%v", err)
	}
}

func TestServiceValidatesPromotion(t *testing.T) {
	repository := &fakeRepository{}
	service, _ := NewService(repository, testCertificates)
	now := time.Now().UTC()
	if _, err := service.PromoteHost(context.Background(), PromoteHostRequest{OperationID: "operation_managed_ssh_01", ActorUserID: "user_01", UserMachineID: "machine_01", MachineGeneration: 1, SetID: "keyset_01234567-89ab-4cde-8fab-0123456789ab", Now: now}); !errors.Is(err, wire.ErrInvalid) {
		t.Fatalf("promotion error=%v", err)
	}

}

func TestServiceValidatesMachineTarget(t *testing.T) {
	repository := &fakeRepository{}
	service, _ := NewService(repository, testCertificates)
	now := time.Unix(1_800_000_000, 0).UTC()
	target, err := service.RegisterTarget(context.Background(), RegisterTargetRequest{OperationID: "operation_managed_ssh_01", ActorUserID: "user_01", UserMachineID: "machine_01", MachineGeneration: 2, OSUser: "deploy", TargetPort: 2222, Now: now})
	if err != nil || target.OSUser != "deploy" || target.TargetPort != 2222 || target.MachineGeneration != 2 {
		t.Fatalf("target=%+v error=%v", target, err)
	}
	for _, user := range []string{"", "-root", `bad"user`, "user@host", "bad\nuser"} {
		if _, err := service.RegisterTarget(context.Background(), RegisterTargetRequest{OperationID: "operation_managed_ssh_01", ActorUserID: "user_01", UserMachineID: "machine_01", MachineGeneration: 2, OSUser: user, TargetPort: 22, Now: now}); !errors.Is(err, wire.ErrInvalid) {
			t.Fatalf("OS user %q error=%v", user, err)
		}
	}
	if _, err := service.UpdateTargetPort(context.Background(), UpdateTargetPortRequest{OperationID: "operation_managed_ssh_01", ActorUserID: "user_01", UserMachineID: "machine_01", MachineGeneration: 2, TargetPort: 22, Now: now}); !errors.Is(err, wire.ErrInvalid) {
		t.Fatalf("missing reconciliation version error=%v", err)
	}
}

func publicLine(t *testing.T, kind string) string {
	t.Helper()
	var public any
	var err error
	if kind == "rsa" {
		var private *rsa.PrivateKey
		private, err = rsa.GenerateKey(rand.Reader, 2048)
		if err == nil {
			public = &private.PublicKey
		}
	} else {
		public, _, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func TestServiceHostSetEntropyFailureDoesNotObserve(t *testing.T) {
	repository := &fakeRepository{}
	service, _ := NewService(repository, testCertificates)
	service.random = strings.NewReader("")
	_, err := service.ObserveHost(context.Background(), ObserveHostRequest{OperationID: "operation_managed_ssh_01", UserID: "user_01", UserMachineID: "machine_01", MachineGeneration: 1, ObservationGeneration: 1, PublicKeys: []string{publicLine(t, "ed25519")}, Now: time.Now()})
	if !errors.Is(err, io.EOF) || repository.host.ID != "" {
		t.Fatalf("entropy failure=%v observed=%q", err, repository.host.ID)
	}
}

// X-28: the same table as the CLI's TestValidUsernameAcceptsSpacesAndUnicodeLettersOnly.
func TestValidOSUserAcceptsSpacesAndUnicodeLettersOnly(t *testing.T) {
	for _, value := range []string{"Zoë Test", "zoë", "東京 ユーザー", "a b", "pb.user_1", `HOST\Zoë Test`, "runneradmin"} {
		if !ValidOSUser(value) {
			t.Errorf("%q refused", value)
		}
	}
	for _, value := range []string{"", " a", "a ", "-a", "a\tb", "a\u00a0b", "a\xffb", `a"b`, "a'b", "a,b", "a*b", "a$b", "a`b", "a%b", "a/b", "a@b", "a;b"} {
		if ValidOSUser(value) {
			t.Errorf("%q accepted", value)
		}
	}
}
