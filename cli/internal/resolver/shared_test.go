package resolver

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"testing"
)

type sharedResolverClient struct {
	*fakeClient
	descriptor api.ConnectionDescriptor
	calls      int
}

func (c *sharedResolverClient) SharedTerminalConnectionDescriptor(context.Context, string) (api.ConnectionDescriptor, error) {
	c.calls++
	return c.descriptor, nil
}
func sharedDescriptor() api.ConnectionDescriptor {
	terminal := readyTerminal()
	terminal.SessionID = "ses_shared"
	terminal.Auth.Method = "bearer"
	terminal.Auth.Token = "test-credential"
	terminal.Auth.Ticket = ""
	terminal.Auth.Scopes = []string{"terminal:view"}
	terminal.Auth.AccessSessionID = "access_shared"
	d := readyUserMachineResponse(terminal)
	d.MachineGeneration = 2
	d.Capabilities = []string{"terminal"}
	d.FileTransfer = nil
	return d
}
func TestSharedResolverRequiresExactSessionRoleAndNoExtraAuthority(t *testing.T) {
	cases := []struct {
		name   string
		change func(*api.ConnectionDescriptor)
	}{
		{"valid", func(*api.ConnectionDescriptor) {}},
		{"wrong session", func(d *api.ConnectionDescriptor) { d.Terminal.SessionID = "other" }},
		{"owner credential", func(d *api.ConnectionDescriptor) { d.Terminal.Auth.Scopes = []string{"terminal:operate"} }},
		{"mixed scopes", func(d *api.ConnectionDescriptor) {
			d.Terminal.Auth.Scopes = []string{"terminal:view", "terminal:control"}
		}},
		{"files", func(d *api.ConnectionDescriptor) { d.FileTransfer = &api.FileTransfer{} }},
		{"extra capability", func(d *api.ConnectionDescriptor) { d.Capabilities = append(d.Capabilities, "exec") }},
		{"missing generation", func(d *api.ConnectionDescriptor) { d.MachineGeneration = 0 }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			client := &sharedResolverClient{fakeClient: &fakeClient{}, descriptor: sharedDescriptor()}
			tt.change(&client.descriptor)
			r := newTestResolver(client.fakeClient)
			r.client = client
			info, err := r.ResolveShared(context.Background(), "ses_shared")
			if tt.name == "valid" {
				if err != nil || !info.Terminal.ViewOnly() {
					t.Fatalf("shared resolve: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid authority accepted")
			}
			if client.calls != 1 {
				t.Fatalf("descriptor calls=%d", client.calls)
			}
		})
	}
}
