package recordingcontrol

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

const (
	reportInterval = 30 * time.Second
	pruneInterval  = time.Hour
	policyInterval = 5 * time.Minute
)

var (
	// ErrUnavailable: the team does not record on this machine (no Teams plan, no team key, or a
	// key this machine cannot verify).
	ErrUnavailable = errors.New("session recording is not available on this machine")
	// ErrForbidden: the actor may not start or stop this recording.
	ErrForbidden = errors.New("you may not start or stop this recording")
)

// Code is the terminal protocol's error code for a start or stop refusal.
func Code(err error) string {
	switch {
	case errors.Is(err, ErrUnavailable), errors.Is(err, session.ErrRecordingUnavailable):
		return wire.ErrorUnavailable
	case errors.Is(err, ErrForbidden):
		return wire.ErrorForbidden
	case errors.Is(err, recordings.ErrRecording):
		return wire.ErrorActive
	case errors.Is(err, recordings.ErrNotRecording):
		return wire.ErrorInactive
	case errors.Is(err, session.ErrSessionUnknown), errors.Is(err, session.ErrInvalidTransition):
		return "not_found_or_forbidden"
	default:
		return "unavailable"
	}
}

// Sessions is the part of the session service recordings use.
type Sessions interface {
	List() []session.Snapshot
	Snapshot(sessionID string) (session.Snapshot, error)
	StartRecording(sessionID string, request recordings.StartRequest) (recordings.State, error)
	StopRecording(sessionID, actorAccountID, reason string) (recordings.Meta, error)
	Recordings() ([]recordings.Meta, error)
	PruneRecordings(retention time.Duration, quotaBytes int64) ([]recordings.Meta, error)
	CloseInterruptedRecording(recordingID string) (recordings.Meta, error)
	RecordingSegment(recordingID string, index uint32) ([]byte, error)
}

type Config struct {
	Sessions Sessions
	Policies *PolicyStore
	// Fetch reads the current policy from Paperboat; Events wakes the controller when it changed.
	Fetch func(context.Context) (wire.Policy, error)
	// Archive and Put copy closed segments to the team bucket; both nil keeps recordings on this machine only.
	Archive Archive
	Put     Put
	Events  interface{ Changed() <-chan struct{} }
	Audit   *offlineaudit.Queue
	// LedgerPath holds which recording reports are already in the audit queue.
	LedgerPath string
	Now        func() time.Time
}

type Controller struct {
	config Config
	ledger *ledger
	kick   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
	// archiveDone closes when archiveLoop returns: uploads never delay the policy and reports in run.
	archiveDone chan struct{}
	// archiveNext is the earliest next archive attempt, archiveFailures the streak behind it (archiveLoop only).
	archiveNext     time.Time
	archiveFailures int
}

func New(config Config) (*Controller, error) {
	if config.Sessions == nil || config.Policies == nil || config.Audit == nil || config.LedgerPath == "" {
		return nil, errors.New("recording control configuration is incomplete")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	ledger, err := openLedger(config.LedgerPath)
	if err != nil {
		return nil, err
	}
	return &Controller{config: config, ledger: ledger, kick: make(chan struct{}, 1)}, nil
}

func (c *Controller) wake() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Controller) request(trigger, actor string, policy Policy) recordings.StartRequest {
	return recordings.StartRequest{Trigger: trigger, ActorAccountID: actor, Key: recordings.Key{Generation: policy.Key.Generation, Public: policy.Key.PublicKey}, QuotaBytes: int64(policy.DiskQuotaMB) << 20}
}

// StartRecording records a session at the request of someone attached to it.
func (c *Controller) StartRecording(sessionID, actorAccountID string) (recordings.State, error) {
	policy := c.config.Policies.Current()
	if !policy.Usable() {
		return recordings.State{}, ErrUnavailable
	}
	if policy.ManualStart != wire.ManualStartAnyone && !policy.Admin(actorAccountID) {
		return recordings.State{}, ErrForbidden
	}
	state, err := c.config.Sessions.StartRecording(sessionID, c.request(recordings.TriggerManual, actorAccountID, policy))
	c.wake()
	return state, err
}

// StopRecording ends a session's recording: a manual one by whoever started it or an admin, one an admin
// rule started only by an admin (the team owner is an admin).
func (c *Controller) StopRecording(sessionID, actorAccountID string) (recordings.Meta, error) {
	snapshot, err := c.config.Sessions.Snapshot(sessionID)
	if err != nil {
		return recordings.Meta{}, err
	}
	current := snapshot.Recording
	if current == nil {
		return recordings.Meta{}, recordings.ErrNotRecording
	}
	starter := current.Trigger == recordings.TriggerManual && current.StartedBy == actorAccountID
	if actorAccountID == "" || !starter && !c.config.Policies.Current().Admin(actorAccountID) {
		return recordings.Meta{}, ErrForbidden
	}
	meta, err := c.config.Sessions.StopRecording(sessionID, actorAccountID, recordings.ReasonStopped)
	c.wake()
	return meta, err
}

// Attached starts a rule recording when someone a rule records attaches to a session, and reports
// whether it started one.
func (c *Controller) Attached(sessionID, accountID string) bool {
	policy := c.config.Policies.Current()
	if !policy.Usable() || !policy.Records(accountID) {
		return false
	}
	snapshot, err := c.config.Sessions.Snapshot(sessionID)
	if err != nil || snapshot.Recording != nil || snapshot.State != session.Running {
		return false
	}
	_, err = c.config.Sessions.StartRecording(sessionID, c.request(recordings.TriggerRule, "", policy))
	if err != nil && !errors.Is(err, recordings.ErrRecording) {
		diagnosticlog.TryWarn("a rule recording could not start", "error_code", Code(err))
	}
	c.wake()
	return err == nil
}

// reconcile applies a policy to the running sessions: a team that stopped recording (plan, owner
// team) stops every recording, a rule recording stops when its rule no longer records anyone attached, and people a
// new rule records are recorded from now on.
func (c *Controller) reconcile(policy Policy) {
	for _, snapshot := range c.config.Sessions.List() {
		if snapshot.State != session.Running {
			continue
		}
		if !policy.Enabled && snapshot.Recording != nil {
			_, _ = c.config.Sessions.StopRecording(snapshot.ID, "", recordings.ReasonPolicy)
			continue
		}
		if snapshot.Recording != nil && snapshot.Recording.Trigger == recordings.TriggerRule && !ruleStillRecords(snapshot, policy) {
			_, _ = c.config.Sessions.StopRecording(snapshot.ID, "", recordings.ReasonPolicy)
			continue
		}
		if snapshot.Recording == nil && policy.Usable() {
			for _, participant := range snapshot.Participants {
				if policy.Records(participant.AccountID) {
					_, _ = c.config.Sessions.StartRecording(snapshot.ID, c.request(recordings.TriggerRule, "", policy))
					break
				}
			}
		}
	}
}

// ruleStillRecords: a rule recording goes on while someone attached is still recorded by the policy; with nobody
// attached it goes on while any account is recorded (the session outlives its viewers).
func ruleStillRecords(snapshot session.Snapshot, policy Policy) bool {
	for _, participant := range snapshot.Participants {
		if policy.Records(participant.AccountID) {
			return true
		}
	}
	return len(snapshot.Participants) == 0 && len(policy.RecordAccounts) > 0
}

func (c *Controller) refreshPolicy(ctx context.Context) {
	if c.config.Fetch == nil {
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	fetched, err := c.config.Fetch(fetchCtx)
	cancel()
	if err != nil {
		diagnosticlog.TryInfo("the recording policy could not be fetched; the cached one stays")
		return
	}
	policy, err := c.config.Policies.Replace(fetched)
	if errors.Is(err, ErrKeyUnverified) {
		diagnosticlog.TryWarn("the team recording key does not follow the pinned key; this machine does not record")
	}
	if policy.Version != 0 || err == nil {
		c.reconcile(policy)
	}
}

// Start runs the controller: the policy on start, on its machine event and every few minutes; the
// reports every 30 s and right after a start or stop; retention every hour.
func (c *Controller) Start(context.Context) error {
	if c.cancel != nil {
		return errors.New("recording control already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel, c.done, c.archiveDone = cancel, make(chan struct{}), make(chan struct{})
	go c.run(ctx)
	go c.archiveLoop(ctx)
	return nil
}

func (c *Controller) Shutdown(ctx context.Context) error {
	if c.cancel == nil {
		return nil
	}
	c.cancel()
	for _, done := range []chan struct{}{c.done, c.archiveDone} {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (c *Controller) run(ctx context.Context) {
	defer close(c.done)
	var changed <-chan struct{}
	if c.config.Events != nil {
		changed = c.config.Events.Changed()
	}
	reports, policies, prunes := time.NewTicker(reportInterval), time.NewTicker(policyInterval), time.NewTicker(pruneInterval)
	defer reports.Stop()
	defer policies.Stop()
	defer prunes.Stop()
	c.refreshPolicy(ctx)
	c.prune()
	c.report()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changed:
			c.refreshPolicy(ctx)
		case <-policies.C:
			c.refreshPolicy(ctx)
		case <-prunes.C:
			c.prune()
		case <-c.kick:
		case <-reports.C:
		}
		c.report()
	}
}
