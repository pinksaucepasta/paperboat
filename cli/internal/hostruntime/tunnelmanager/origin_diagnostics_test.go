package tunnelmanager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func TestOriginFailureDiagnosticSurvivesQuietProcessLogger(t *testing.T) {
	var output bytes.Buffer
	prior, priorDefault := originDiagnosticLogger, slog.Default()
	originDiagnosticLogger = slog.New(slog.NewTextHandler(&output, nil))
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer func() { originDiagnosticLogger = prior; slog.SetDefault(priorDefault) }()
	logOriginStreamFailure(context.Background(), "http_read", io.ErrUnexpectedEOF)
	if !strings.Contains(output.String(), "stage=http_read code=unexpected_eof") {
		t.Fatalf("missing finite failure category: %s", output.String())
	}
}

// Every admitIngress refusal names its stage in the origin diagnostic log and
// nothing about the request, route or principal.
func TestAdmitIngressRefusalLogsItsStage(t *testing.T) {
	decision, open := browserDecisionFixture(time.Now().UTC())
	route := hoststate.TunnelConfigRoute{ID: decision.Binding.RouteID, Protocol: "http", OriginScheme: "http", OriginAddress: decision.Binding.OriginAddress, TLSVerification: "not_applicable"}
	authority := func(answer connectorprotocol.IngressDecision, err error) OriginStreamForwarder {
		return OriginStreamForwarder{IngressAuthority: func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
			return answer, err
		}}
	}
	public := decision
	public.Binding.Audience = "public"
	public.PrincipalID, public.GrantID, public.GrantGeneration, public.MembershipGeneration = "", "", 0, 0
	otherRoute := route
	otherRoute.OriginAddress = "127.0.0.1:3001"
	otherDecision := decision
	otherDecision.PolicyGeneration = 2
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	cases := []struct {
		stage   string
		ctx     context.Context
		sent    connectorprotocol.IngressDecision
		garbage bool
		forward OriginStreamForwarder
		route   hoststate.TunnelConfigRoute
	}{
		{stage: "decision_read", ctx: t.Context(), garbage: true, forward: authority(decision, nil), route: route},
		{stage: "audience", ctx: t.Context(), sent: public, forward: authority(public, nil), route: route},
		{stage: "authority_lookup", ctx: t.Context(), sent: decision, forward: authority(connectorprotocol.IngressDecision{}, errors.New("lookup failed")), route: route},
		{stage: "authority_binding", ctx: t.Context(), sent: decision, forward: authority(otherDecision, nil), route: route},
		{stage: "origin_binding", ctx: t.Context(), sent: decision, forward: authority(decision, nil), route: otherRoute},
		{stage: "admission", ctx: canceled, sent: decision, forward: authority(decision, nil), route: route},
	}
	for _, c := range cases {
		t.Run(c.stage, func(t *testing.T) {
			var output bytes.Buffer
			prior := originDiagnosticLogger
			originDiagnosticLogger = slog.New(slog.NewTextHandler(&output, nil))
			defer func() { originDiagnosticLogger = prior }()
			input, stream := net.Pipe()
			defer stream.Close()
			go func() {
				if c.garbage {
					_, _ = input.Write([]byte("not a decision\n"))
				} else {
					_ = connectorprotocol.WriteIngressDecision(input, c.sent, time.Now().UTC())
				}
				_ = input.Close()
			}()
			if _, _, err := c.forward.admitIngress(c.ctx, stream, open, c.route); err == nil {
				t.Fatal("ingress admitted")
			}
			logged := output.String()
			if !strings.Contains(logged, "durable ingress rejected") || !strings.Contains(logged, "stage="+c.stage) {
				t.Fatalf("log = %q, want stage=%s", logged, c.stage)
			}
			for _, private := range []string{decision.Binding.Hostname, decision.Binding.OriginAddress, decision.PrincipalID, decision.GrantID, "lookup failed"} {
				if strings.Contains(logged, private) {
					t.Fatalf("log carries request detail %q: %s", private, logged)
				}
			}
		})
	}
}
