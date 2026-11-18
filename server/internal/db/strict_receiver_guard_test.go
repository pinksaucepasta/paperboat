package db_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// strictDecoders lists every server function that still refuses unknown JSON fields, with why. Requests
// from released pb, pbh, edge and relay builds must ignore fields a newer build adds (from launch on the
// server serves the current and previous release); only these may stay strict: signed authorization
// claims, values the server minted itself, operator/console bodies (our web apps are always current),
// and local files. A new strict decoder on a receiver path fails TestNoNewStrictReceivers. Because an older
// server ignores what it does not know, a new field that narrows access must be fail-closed: a new route
// or a capability the sender checks first (shared/capver), never a silently ignorable body field.
var strictDecoders = map[string]string{
	"cmd/paperboat-watchdog/main.go:run":                                           "operator config file",
	"internal/config/env.go:overlayEnv":                                            "server configuration",
	"internal/controlplane/config_repository_access.go:replay":                     "server-minted grant read back from its own database",
	"internal/controlplane/helper_proof.go:strictProofJSON":                        "signed helper proof claims",
	"internal/controlplane/machine_proof.go:strictCanonicalMachineJSON":            "signed machine proof claims",
	"internal/controlplane/signed_usage.go:strictUsageJSON":                        "signed edge usage document",
	"internal/httpapi/account_deletion_handlers.go:accountDelete":                  "browser-only body",
	"internal/httpapi/device_auth_handlers.go:decodeBody":                          "decodeStrictJSON only: web-app bodies and E2EE/identity tripwires; decodeClientJSON is tolerant",
	"internal/httpapi/browser_access_handlers.go:browserDecode":                    "browser-only bodies (refuses CLI principals)",
	"internal/httpapi/client_update_policy.go:clientUpdatePolicyPut":               "admin body",
	"internal/httpapi/operator_auth_handlers.go:operatorSetupRequest":              "admin body",
	"internal/httpapi/operator_mfa_handlers.go:registerOperatorMFA":                "admin body",
	"internal/httpapi/release_authority_handlers.go:releaseAuthorityRequestCreate": "admin body",
	"internal/mint/mint.go:strictCredentialJSON":                                   "server-minted credential claims",
	"internal/previewdomain/service.go:Decode":                                     "server-minted signed cursor",
	"internal/previewtunnelapi/common.go:Decode":                                   "server-minted signed cursor",
	"internal/previewv1/service.go:Decode":                                         "server-minted signed cursor",
	"internal/releaseauthority/service.go:Decode":                                  "threshold-signed release bundle",
	"internal/releases/current.go:validAssetTargetCustom":                          "our own TUF publication; released updaters before 2026.11.18 refuse unknown fields",
	"internal/servernodes/enroll.go:ServeHTTP":                                     "server-node bootstrap between server builds of one deploy",
	"internal/tunnelv1/cursor.go:Decode":                                           "server-minted signed cursor",
	"internal/tunnelv1/resources_service.go:decode":                                "server-minted signed cursor",
	"internal/usermachines/machine_control.go:strictMachineJSON":                   "signed machine proof claims",
	"tools/scenario-matrix/main.go:build":                                          "tool input file",
}

func TestNoNewStrictReceivers(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", ".."))
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if name := entry.Name(); name == ".git" || name == "vendor" || name == ".build" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if package_, _ := selector.X.(*ast.Ident); selector.Sel.Name == "DisallowUnknownFields" || package_ != nil && package_.Name == "strictjson" && selector.Sel.Name == "Decode" {
					found[filepath.ToSlash(relative)+":"+function.Name.Name] = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var unlisted, stale []string
	for site := range found {
		if strictDecoders[site] == "" {
			unlisted = append(unlisted, site)
		}
	}
	for site := range strictDecoders {
		if !found[site] {
			stale = append(stale, site)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	if len(unlisted) > 0 {
		t.Errorf("strict JSON decoders not in strictDecoders (receivers of pb/pbh/edge/relay must ignore unknown fields; list only claims, own values, web-app bodies and files):\n%s", strings.Join(unlisted, "\n"))
	}
	if len(stale) > 0 {
		t.Errorf("strictDecoders entries no longer found, remove them:\n%s", strings.Join(stale, "\n"))
	}
}
