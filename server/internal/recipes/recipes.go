// Package recipes owns recipes (PLAN F26): a recipe pairs and sets up machines with one token,
// acting as the member who created it in one workspace. The token is made on the creator's
// computer (shared/recipe) so the vault keys can be wrapped to it; the server stores only
// sha256(auth) and can admit a machine but never decrypt a vault.
package recipes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/policy"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

var (
	ErrNotFound  = errors.New("recipe not found")
	ErrNameTaken = errors.New("recipe name taken")
	// ErrForbidden: team recipes are created and changed by owners and admins.
	ErrForbidden    = errors.New("recipes in a team are managed by owners and admins")
	ErrInvalidInput = errors.New("invalid recipe request")
	ErrTokenTaken   = errors.New("recipe token id taken")
	ErrInvalidPage  = errors.New("invalid recipe page")
	ErrRevoked      = errors.New("recipe revoked")
)

// FileError is a recipe file that failed shared/recipe validation.
type FileError struct{ Errors recipe.Errors }

func (e *FileError) Error() string { return e.Errors.Error() }

// ReachError is a [[reach]] entry the workspace policy does not resolve (unknown tag, machine).
type ReachError struct{ Errors policy.Errors }

func (e *ReachError) Error() string { return "recipe reach: " + e.Errors.Error() }

// NameError is a vault or repository name the workspace does not have. Known lists what it has.
type NameError struct {
	Kind, Name, Workspace, Suggestion string
	Known                             []string
}

func (e *NameError) Error() string {
	var b strings.Builder
	switch e.Kind {
	case "vault":
		fmt.Fprintf(&b, "vault %q does not exist in workspace %s.", e.Name, e.Workspace)
		if len(e.Known) > 0 {
			fmt.Fprintf(&b, " Your vaults: %s.", strings.Join(e.Known, ", "))
		}
	default:
		fmt.Fprintf(&b, "repository %q is not connected in workspace %s.", e.Name, e.Workspace)
		if len(e.Known) > 0 {
			fmt.Fprintf(&b, " Connected: %s.", strings.Join(e.Known, ", "))
		}
	}
	if e.Suggestion != "" {
		fmt.Fprintf(&b, " Did you mean %q?", e.Suggestion)
	}
	if e.Kind == "repository" {
		b.WriteString(" Connect one with pb config repository connect <url>.")
	}
	return b.String()
}

// ConfirmError: rotate and revoke need the code the impact was shown with.
type ConfirmError struct {
	Code   string
	Impact Impact
}

func (e *ConfirmError) Error() string { return "confirmation required: " + e.Code }

// Impact is what a rotate or revoke changes, shown before it runs.
type Impact struct {
	Machines []MachineRef `json:"machines"`
	Vaults   []string     `json:"vaults"`
}

type MachineRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
}

// Recipe is one stored recipe as the API shows it. CreatedBy is "user:<email>".
type Recipe struct {
	ID          string
	TokenID     string
	AccountID   string
	Workspace   workspace.Scope
	Name        string
	Revision    int
	Source      string
	Format      string
	Recipe      recipe.Recipe
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time
	LastUsedAt  *time.Time
	RotatedAt   *time.Time
	RevokedAt   *time.Time
	Machines    int
	MaxMachines int
}

// Input is a recipe file as the client sends it, with the token it made (create and rotate).
type Input struct {
	Name   string
	Source string
	Format string // toml or json
	// TokenID and Auth come from shared/recipe.Token on the creator's computer.
	TokenID string
	Auth    []byte
	// Wraps carry each vault key the recipe newly reads, wrapped to the recipe's public key.
	Wraps []VaultWrap
}

// MachineRemover removes a machine the way pb machine revoke does (usermachines.Service.Delete).
type MachineRemover func(ctx context.Context, account, machineID string) error

type Service struct {
	db     *db.DB
	audit  *audit.Writer
	remove MachineRemover
	now    func() time.Time
}

func New(store *db.DB, writer *audit.Writer, remove MachineRemover) *Service {
	return &Service{db: store, audit: writer, remove: remove, now: time.Now}
}

// parse validates the file with shared/recipe; the name comes from the request.
func parse(in Input) (recipe.Recipe, error) {
	if !recipe.ValidName(in.Name) || (in.Format != "toml" && in.Format != "json") {
		return recipe.Recipe{}, ErrInvalidInput
	}
	r, err := recipe.Parse(in.Name+"."+in.Format, []byte(in.Source))
	var errs recipe.Errors
	if errors.As(err, &errs) {
		return recipe.Recipe{}, &FileError{Errors: errs}
	}
	return r, err
}

func checkToken(in Input) error {
	if !recipe.ValidID(in.TokenID) || len(in.Auth) != 32 {
		return ErrInvalidInput
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "recipe_" + hex.EncodeToString(b), nil
}

// confirmCode binds a confirmation to the recipe state the impact was computed from.
func confirmCode(action string, r Recipe, impact Impact) string {
	h := sha256.New()
	rotated := ""
	if r.RotatedAt != nil {
		rotated = r.RotatedAt.UTC().Format(time.RFC3339Nano)
	}
	fmt.Fprintf(h, "%s\x00%s\x00%d\x00%s", action, r.ID, r.Revision, rotated)
	for _, m := range impact.Machines {
		fmt.Fprintf(h, "\x00%s", m.ID)
	}
	return base32.StdEncoding.EncodeToString(h.Sum(nil))[:6]
}

func scopeFor(teamID string) workspace.Scope {
	if teamID == "" {
		return workspace.Scope{ID: "personal"}
	}
	return workspace.Scope{ID: teamID, TeamID: teamID}
}
