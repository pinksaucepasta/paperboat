package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

// wasmPeer talks to a built wasm module through testdata/harness.mjs.
type wasmPeer struct {
	t      *testing.T
	in     *json.Encoder
	out    *bufio.Scanner
	finish func()
}

func startWasmPeer(t *testing.T) *wasmPeer {
	wasm, glue := os.Getenv("PB_ENVVAULT_WASM"), os.Getenv("PB_ENVVAULT_WASM_EXEC")
	if wasm == "" || glue == "" {
		t.Skip("set PB_ENVVAULT_WASM and PB_ENVVAULT_WASM_EXEC to cross-check a built wasm module")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the wasm cross-check")
	}
	cmd := exec.Command(node, "testdata/harness.mjs", wasm, glue)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	out := bufio.NewScanner(stdout)
	out.Buffer(make([]byte, 1<<20), 8<<20)
	return &wasmPeer{t: t, in: json.NewEncoder(stdin), out: out, finish: func() { stdin.Close(); _ = cmd.Wait() }}
}

func (p *wasmPeer) call(r map[string]any) map[string]any {
	p.t.Helper()
	if err := p.in.Encode(r); err != nil {
		p.t.Fatal(err)
	}
	if !p.out.Scan() {
		p.t.Fatalf("wasm peer closed: %v", p.out.Err())
	}
	var reply struct {
		Data  map[string]any `json:"data"`
		Error string         `json:"error"`
	}
	if err := json.Unmarshal(p.out.Bytes(), &reply); err != nil {
		p.t.Fatal(err)
	}
	if reply.Error != "" {
		p.t.Fatalf("wasm %v: %s", r["action"], reply.Error)
	}
	return reply.Data
}

// TestBuiltWasmInteroperatesWithNativeCode proves a built module (TinyGo or Go)
// produces and accepts exactly the bytes the native CLI code does.
func TestBuiltWasmInteroperatesWithNativeCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	peer := startWasmPeer(t)
	defer peer.finish()

	// Native vault -> wasm unlock.
	e, v := initialized(t)
	defer e.clear()
	vault := map[string]any{"issuer": v.Issuer, "account_id": v.Account, "generation": v.Generation, "document_id": v.DocumentID, "envelope": v.Envelope}
	if peer.call(map[string]any{"action": "unlock", "vault": vault, "password": "browser password"})["unlocked"] != true {
		t.Fatal("wasm did not unlock a natively sealed vault")
	}
	// Wasm-sealed scope anchor, record and host layer -> native open.
	anchorRaw := peer.call(map[string]any{"action": "scope-create", "vault": vault, "workspace_id": "personal", "kind": "personal", "owner": v.Account, "machine": ""})["envelope"].(string)
	protection, err := env.PasswordVaultProtection(e.raw)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := decode(anchorRaw)
	anchorDoc, err := env.ParseVaultScope(raw, protection.WriterPublic)
	if err != nil {
		t.Fatalf("native code rejected the wasm scope anchor: %v", err)
	}
	c := anchorDoc.Claims
	anchor := scopeState{WorkspaceID: c.WorkspaceID, WriterPublic: encoded(protection.WriterPublic), Kind: c.OwnerKind, Owner: c.OwnerID, Machine: c.MachineID, Epoch: c.KeyEpoch, Revision: c.Revision, DocumentID: digest(anchorDoc.ID), Envelope: anchorRaw}
	scope := map[string]any{"workspace_id": anchor.WorkspaceID, "writer_public": anchor.WriterPublic, "owner_kind": anchor.Kind, "owner_id": anchor.Owner, "machine_id": anchor.Machine, "key_epoch": anchor.Epoch, "revision": anchor.Revision, "document_id": anchor.DocumentID, "envelope": anchor.Envelope}
	written := peer.call(map[string]any{"action": "record-write", "vault": vault, "scope": scope, "workspace_id": "personal", "kind": "personal", "owner": v.Account, "machine": "", "name": "WASM_NAME", "value": "wasm value"})
	recordRaw, _ := decode(written["envelope"].(string))
	record, err := env.ParseVaultRecord(recordRaw, protection.WriterPublic)
	if err != nil {
		t.Fatalf("native code rejected the wasm record: %v", err)
	}
	key, err := env.VaultRecordKey(e.keys.PersonalKey, "personal", "personal", v.Account, "", e.keys.PersonalEpoch)
	if err != nil {
		t.Fatal(err)
	}
	name, value, err := env.OpenVaultRecord(ctx, record, key)
	if err != nil || name != "WASM_NAME" || string(value) != "wasm value" {
		t.Fatalf("native open of wasm record: %q %q %v", name, value, err)
	}
	recipient, private := hostRecipient(t, v.Account, "machine-one")
	layerOut := peer.call(map[string]any{"action": "host-layer", "vault": vault, "scope": scope, "workspace_id": "personal", "operation_id": "operation_x", "recipient_metadata": recipient})
	layerRaw, _ := decode(layerOut["envelope"].(string))
	layer, err := env.ParseVaultLayer(layerRaw, protection.WriterPublic)
	if err != nil {
		t.Fatalf("native code rejected the wasm layer: %v", err)
	}
	opened, err := env.OpenVaultScopeKey(ctx, layer, layer.Claims, private.Bytes())
	if err != nil || string(opened) != string(key) {
		t.Fatalf("native open of wasm layer failed: %v", err)
	}
	// Native record -> wasm record-name (and the reply holds no value).
	nativeAnchor := createAnchor(t, e, v, "personal", "personal", v.Account, "")
	nativeRecord := writeRecord(t, e, v, nativeAnchor, "NATIVE_NAME", "native value", nil)
	named := peer.call(map[string]any{"action": "record-name", "vault": vault, "record": nativeRecord})
	if named["name"] != "NATIVE_NAME" || named["deleted"] != false || len(named) != 2 {
		t.Fatalf("wasm record-name reply: %v", named)
	}
	// Wasm-initialized vault -> native unlock.
	fresh := map[string]any{"issuer": v.Issuer, "account_id": "account-two"}
	sealed := peer.call(map[string]any{"action": "initialize", "vault": fresh, "password": "second password"})
	rawVault, _ := decode(sealed["envelope"].(string))
	head, _, err := env.InspectPasswordVault(rawVault)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := env.OpenPasswordVault(ctx, head, []byte("second password"), rawVault)
	if err != nil {
		t.Fatalf("native unlock of wasm vault: %v", err)
	}
	clear(payload)
	if _, err := env.OpenPasswordVault(ctx, head, []byte("wrong"), rawVault); err == nil {
		t.Fatal("native code opened the wasm vault with a wrong password")
	}
	peer.call(map[string]any{"action": "lock"})
}
