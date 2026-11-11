//go:build darwin || linux || windows

package runtime

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordingcontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// productionRecordingControl builds the team recordings controller: the policy from Paperboat with
// the machine's credential, kept under the runtime state, and reports through the audit queue.
func productionRecordingControl(controlURL *url.URL, stateRoot string, transport http.RoundTripper, tokens *enrollment.RenewingTokenSource, operationID func() (string, error), events *machineevents.Hub, audit *offlineaudit.Queue) func(session.Service) (*recordingcontrol.Controller, error) {
	return func(sessions session.Service) (*recordingcontrol.Controller, error) {
		policies, err := recordingcontrol.OpenPolicyStore(filepath.Join(stateRoot, "runtime", "recording-policy.json"))
		if err != nil {
			return nil, err
		}
		fetcher := &recordingcontrol.Fetcher{ServerURL: strings.TrimSuffix(controlURL.String(), "/"),
			Client: &http.Client{Transport: errorreport.TransportOperation(transport, controlURL.String(), "recording_policy"), CheckRedirect: rejectRuntimePolicyRedirect},
			Tokens: tokens, Proofs: enrollment.ProofSource{StateRoot: stateRoot}, OperationID: operationID}
		return recordingcontrol.New(recordingcontrol.Config{Sessions: sessions, Policies: policies, Fetch: fetcher.Fetch,
			Events: events.Subscribe(machineevents.TopicRecordingPolicy), Audit: audit,
			LedgerPath: filepath.Join(stateRoot, "runtime", "recording-reports.json")})
	}
}
