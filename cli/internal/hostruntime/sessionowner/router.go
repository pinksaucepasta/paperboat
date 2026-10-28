package sessionowner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

// OwnerCall is an authenticated call to one generation's session owner.
type OwnerCall = session.RemoteCall

// Owner binds a previously authenticated owner connection to its descriptor.
// Call and Identity may be empty in catalog entries; Ensure must then return a
// currently authenticated owner before that generation can be used.
type Owner struct {
	Descriptor Descriptor
	Identity   workloadbridge.OwnerIdentity
	Call       OwnerCall
}

// Ensure resolves or starts one catalogued owner and authenticates the client
// against the returned descriptor before returning it.
type Ensure func(context.Context, string) (Owner, error)

type routedSession struct {
	generation string
	name       string
	state      session.State
}

// liveSessions counts sessions that can still hold a process. Closed records
// are retained only for display; retired generations cannot always delete
// them, so they must not consume the live-session limit.
func liveSessions(sessions map[string]routedSession) int {
	live := 0
	for _, routed := range sessions {
		if routed.state != session.Closed && routed.state != session.Deleted {
			live++
		}
	}
	return live
}

// Router combines generation-scoped session services while keeping each
// session bound to the store in which it was created.
type Router struct {
	admission   sync.Mutex
	mu          sync.RWMutex
	active      string
	owners      map[string]Owner
	sessions    map[string]routedSession
	revision    uint64
	maxIDs      int
	maxOwners   int
	unavailable map[string]time.Time
	ensure      Ensure
	archive     func(context.Context, string) (workloadbridge.ArchiveMetadata, error)
}

// NewRouter creates a router over a complete, bounded owner catalog. Owners
// may be metadata-only entries; Ensure supplies authenticated calls lazily.
func NewRouter(active string, owners map[string]Owner, maxSessions int, ensure Ensure) (*Router, error) {
	if maxSessions < 1 || ensure == nil {
		return nil, session.ErrInvalidSession
	}
	r := &Router{
		maxIDs:    maxSessions,
		maxOwners: maxSessions + 1,
		ensure:    ensure,
		sessions:  make(map[string]routedSession),
	}
	if err := r.setOwners(active, owners); err != nil {
		return nil, err
	}
	return r, nil
}

// SetOwners replaces the active generation and the complete bounded catalog.
func (r *Router) SetOwners(active string, owners map[string]Owner) error {
	r.admission.Lock()
	defer r.admission.Unlock()
	return r.setOwners(active, owners)
}

func (r *Router) setOwners(active string, owners map[string]Owner) error {
	if active == "" || len(owners) == 0 || len(owners) > r.maxOwners {
		return session.ErrInvalidSession
	}
	copyOwners := make(map[string]Owner, len(owners))
	for generation, owner := range owners {
		if generation == "" || owner.Descriptor.Generation != generation || owner.Descriptor.Validate() != nil {
			return session.ErrInvalidSession
		}
		if identityEmpty(owner.Identity) {
			if owner.Call != nil {
				return session.ErrInvalidSession
			}
		} else if !validOwnerIdentity(generation, owner.Descriptor, owner.Identity) {
			return session.ErrInvalidSession
		}
		copyOwners[generation] = owner
	}
	activeOwner, exists := copyOwners[active]
	if !exists {
		return session.ErrInvalidSession
	}
	for _, owner := range copyOwners {
		if !sameAccount(owner.Descriptor, activeOwner.Descriptor) {
			return session.ErrInvalidSession
		}
	}
	r.mu.Lock()
	r.active = active
	r.owners = copyOwners
	r.sessions = make(map[string]routedSession)
	r.unavailable = make(map[string]time.Time)
	r.revision++
	r.mu.Unlock()
	return nil
}

// Service adapts the router to the session Service contract through its
// existing authenticated remote-call boundary.
func (r *Router) Service() (*session.RemoteManager, error) {
	return session.NewRemoteManager(r.Call)
}

// Call routes the session wire protocol to the owner of each session ID.
func (r *Router) Call(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
	if ctx == nil {
		return nil, session.ErrInvalidSession
	}
	switch op {
	case "List":
		var args struct{}
		if err := decodeRemote(body, &args); err != nil {
			return nil, session.ErrInvalidSession
		}
		image, err := r.inspect(ctx, false)
		if err != nil {
			return nil, err
		}
		if err := r.installIndex(image); err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 []session.Snapshot }{image.snapshots})
	case "ResourceCounts":
		var args struct{}
		if err := decodeRemote(body, &args); err != nil {
			return nil, session.ErrInvalidSession
		}
		image, err := r.inspect(ctx, true)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct{ V0 map[string]uint64 }{image.counts})
	case "Create":
		return r.create(ctx, body)
	case "Delete", "DeleteAtGeneration", "Restart", "RestartAtGeneration", "RestartContext", "RestartAtGenerationContext":
		r.admission.Lock()
		defer r.admission.Unlock()
		return r.mutateExisting(ctx, op, body)
	case "Attach", "AttachParticipant", "AttachParticipantAtGeneration", "AttachLive", "AttachLiveParticipant", "AttachLiveParticipantAtGeneration":
		r.admission.Lock()
		defer r.admission.Unlock()
		return r.attach(ctx, op, body)
	case "Snapshot", "SnapshotAtGeneration":
		return r.snapshot(ctx, op, body)
	case "BrowserScreenAttachment", "Detach", "DetachParticipantAtGeneration", "AttachmentOwnedBy", "Next", "WaitNext", "Acknowledge", "AcknowledgeAtGeneration", "AttachmentStatus", "Write", "Interrupt", "InputSequence", "WriteStream", "QueryInput", "Resize", "ResizeParticipantAtGeneration", "ActivateParticipantAtGeneration", "Signal", "Clear", "ClearAtGeneration", "Close", "CloseAtGeneration":
		return r.forwardBySession(ctx, op, body)
	default:
		return nil, session.ErrInvalidSession
	}
}

type routerImage struct {
	revision  uint64
	owners    map[string]Owner
	snapshots []session.Snapshot
	byID      map[string]routedSession
	counts    map[string]uint64
}

func (r *Router) inspect(ctx context.Context, includeCounts bool) (routerImage, error) {
	active, owners, revision := r.catalogSnapshot()
	if active == "" || len(owners) == 0 || len(owners) > r.maxOwners {
		return routerImage{}, session.ErrOwnerUnavailable
	}
	image := routerImage{
		revision:  revision,
		owners:    make(map[string]Owner, len(owners)),
		snapshots: make([]session.Snapshot, 0),
		byID:      make(map[string]routedSession),
		counts:    make(map[string]uint64),
	}
	generations := sortedOwnerKeys(owners)
	for _, generation := range generations {
		var out struct {
			V0 []session.Snapshot
		}
		var archived bool
		var counts map[string]uint64
		if generation != active && r.archive != nil {
			metadata, err := r.archive(ctx, generation)
			if err != nil {
				return routerImage{}, err
			}
			if metadata.Generation != generation {
				return routerImage{}, session.ErrOwnerUnavailable
			}
			archived, out.V0 = metadata.Available, metadata.Snapshots
			if archived {
				counts = map[string]uint64{"sessions": uint64(len(out.V0)), "processes": 0, "attachments": 0}
			}
		}
		if !archived {
			owner, snapshots, resources, err := r.inspectOwner(ctx, generation, owners[generation], includeCounts)
			if ctx.Err() != nil {
				return routerImage{}, ctx.Err()
			}
			var unreachable ownerStartFailure
			if generation != active && (errors.As(err, &unreachable) || errors.Is(err, context.DeadlineExceeded)) {
				// A hung owner is temporarily removed from routing, never killed:
				// its shells may still be running. Retry health after the cooldown.
				r.mu.Lock()
				if !time.Now().Before(r.unavailable[generation]) {
					r.unavailable[generation] = time.Now().Add(ownerRetryInterval)
				}
				r.mu.Unlock()
				continue
			}
			if err != nil {
				return routerImage{}, err
			}
			image.owners[generation] = owner
			out.V0, counts = snapshots, resources
		}
		if out.V0 == nil {
			return routerImage{}, session.ErrOwnerUnavailable
		}
		// One owner holds at most the session limit of live sessions; closed records are retained
		// for display and do not count. A list beyond the retained bound is malformed.
		if len(out.V0) > maxRetainedRecordsPerOwner {
			return routerImage{}, session.ErrResourceLimit
		}
		for _, snapshot := range out.V0 {
			if snapshot.ID == "" || snapshot.Name == "" {
				return routerImage{}, session.ErrOwnerUnavailable
			}
			if _, exists := image.byID[snapshot.ID]; exists {
				return routerImage{}, session.ErrOwnerUnavailable
			}
			for _, existing := range image.snapshots {
				if existing.Name == snapshot.Name {
					return routerImage{}, session.ErrSessionExists
				}
			}
			if len(image.byID) >= maxRetainedRecordsPerOwner*r.maxOwners {
				return routerImage{}, session.ErrResourceLimit
			}
			image.byID[snapshot.ID] = routedSession{generation: generation, name: snapshot.Name, state: snapshot.State}
			image.snapshots = append(image.snapshots, snapshot)
		}
		if includeCounts {
			for key, value := range counts {
				if math.MaxUint64-image.counts[key] < value {
					return routerImage{}, session.ErrResourceLimit
				}
				image.counts[key] += value
			}
		}
	}
	sort.Slice(image.snapshots, func(i, j int) bool {
		if image.snapshots[i].Name == image.snapshots[j].Name {
			return image.snapshots[i].ID < image.snapshots[j].ID
		}
		return image.snapshots[i].Name < image.snapshots[j].Name
	})
	return image, nil
}

// Bound the whole health/inventory exchange, like derphttp.Client.connect's
// context deadline (ref/tailscale/derp/derphttp/derphttp_client.go).
const ownerInspectionTimeout = 2 * time.Second
const ownerRetryInterval = 30 * time.Second

func (r *Router) inspectOwner(
	ctx context.Context, generation string, expected Owner, includeCounts bool,
) (Owner, []session.Snapshot, map[string]uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, ownerInspectionTimeout)
	defer cancel()
	owner, err := r.ensureOwner(ctx, generation, expected)
	if err != nil {
		return Owner{}, nil, nil, err
	}
	body, err := owner.Call(ctx, "List", json.RawMessage(`{}`))
	if err != nil {
		return Owner{}, nil, nil, err
	}
	var out struct{ V0 []session.Snapshot }
	if err := decodeRemote(body, &out); err != nil {
		return Owner{}, nil, nil, errors.Join(session.ErrOwnerUnavailable, err)
	}
	var counts map[string]uint64
	if includeCounts {
		counts, err = remoteResourceCounts(ctx, owner.Call)
	}
	return owner, out.V0, counts, err
}

func remoteResourceCounts(ctx context.Context, call OwnerCall) (map[string]uint64, error) {
	body, err := call(ctx, "ResourceCounts", json.RawMessage(`{}`))
	if err != nil {
		return nil, err
	}
	var out struct {
		V0 map[string]uint64
	}
	if err := decodeRemote(body, &out); err != nil {
		return nil, errors.Join(session.ErrOwnerUnavailable, err)
	}
	if out.V0 == nil {
		return nil, session.ErrOwnerUnavailable
	}
	// Manager omits resource dimensions whose count is zero. Treat omitted
	// dimensions as zero so every healthy owner has the same aggregate shape.
	for _, key := range []string{"sessions", "attachments", "processes"} {
		if _, exists := out.V0[key]; !exists {
			out.V0[key] = 0
		}
	}
	return out.V0, nil
}

func (r *Router) installIndex(image routerImage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.revision != image.revision {
		return session.ErrOwnerUnavailable
	}
	r.sessions = image.byID
	return nil
}

func (r *Router) create(ctx context.Context, body json.RawMessage) (json.RawMessage, error) {
	r.admission.Lock()
	defer r.admission.Unlock()
	var args struct {
		Request            session.CreateRequest
		ManagedEnvironment []string
	}
	if err := decodeRemote(body, &args); err != nil {
		return nil, session.ErrInvalidSession
	}
	image, err := r.inspect(ctx, false)
	if err != nil {
		return nil, err
	}
	for _, snapshot := range image.snapshots {
		if snapshot.Name == args.Request.Name {
			return nil, session.ErrSessionExists
		}
	}
	if r.atCeiling(image) {
		return nil, session.ErrResourceLimit
	}
	active, _, _ := r.catalogSnapshot()
	owner, ok := image.owners[active]
	if !ok {
		return nil, session.ErrOwnerUnavailable
	}
	response, err := owner.Call(ctx, "Create", body)
	if err != nil {
		return nil, err
	}
	var out struct {
		V0 session.Snapshot
	}
	if err := decodeRemote(response, &out); err != nil || out.V0.ID == "" || out.V0.Name != args.Request.Name {
		return nil, errors.Join(session.ErrOwnerUnavailable, err)
	}
	if _, exists := image.byID[out.V0.ID]; exists || nameExists(image.snapshots, out.V0.Name) {
		cleanupErr := deleteCreated(ctx, owner.Call, out.V0.ID)
		return nil, errors.Join(session.ErrSessionExists, cleanupErr)
	}
	if r.atCeiling(image) {
		cleanupErr := deleteCreated(ctx, owner.Call, out.V0.ID)
		return nil, errors.Join(session.ErrResourceLimit, cleanupErr)
	}
	image.byID[out.V0.ID] = routedSession{generation: active, name: out.V0.Name, state: out.V0.State}
	image.snapshots = append(image.snapshots, out.V0)
	if err := r.installIndex(image); err != nil {
		return nil, err
	}
	return response, nil
}

func deleteCreated(ctx context.Context, call OwnerCall, id string) error {
	body, _ := json.Marshal(struct{ Sessionid string }{id})
	_, err := call(ctx, "Delete", body)
	return err
}

func (r *Router) mutateExisting(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
	id, err := remoteSessionID(body)
	if err != nil {
		return nil, err
	}
	image, err := r.inspect(ctx, false)
	if err != nil {
		return nil, err
	}
	routed, ok := image.byID[id]
	if !ok {
		return nil, session.ErrSessionUnknown
	}
	owner, ok := image.owners[routed.generation]
	if !ok {
		owner, err = r.ensureForGeneration(ctx, routed.generation)
		if err != nil {
			return nil, err
		}
	}
	response, err := owner.Call(ctx, op, body)
	if err != nil {
		return nil, err
	}
	if isDelete(op) {
		delete(image.byID, id)
		for index, snapshot := range image.snapshots {
			if snapshot.ID == id {
				image.snapshots = append(image.snapshots[:index], image.snapshots[index+1:]...)
				break
			}
		}
	} else {
		var out struct {
			V0 session.Snapshot
		}
		if err := decodeRemote(response, &out); err != nil || out.V0.ID != id {
			return nil, errors.Join(session.ErrOwnerUnavailable, err)
		}
		image.byID[id] = routedSession{generation: routed.generation, name: out.V0.Name, state: out.V0.State}
		for index, snapshot := range image.snapshots {
			if snapshot.ID == id {
				image.snapshots[index] = out.V0
				break
			}
		}
	}
	if err := r.installIndex(image); err != nil {
		return nil, err
	}
	return response, nil
}

func (r *Router) attach(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
	image, err := r.inspect(ctx, false)
	if err != nil {
		return nil, err
	}
	return r.forwardFromImage(ctx, op, body, image)
}

func (r *Router) forwardBySession(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
	image, err := r.sessionImageFor(ctx, body)
	if err != nil {
		return nil, err
	}
	return r.forwardFromImage(ctx, op, body, image)
}

// Snapshot observes stopped history without reviving its native process. Replay
// and mutations still require the authenticated owner of the session's store.
func (r *Router) snapshot(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
	var id string
	var expected uint64
	if op == "Snapshot" {
		var args struct{ Sessionid string }
		if err := decodeRemote(body, &args); err != nil {
			return nil, session.ErrInvalidSession
		}
		id = args.Sessionid
	} else {
		var args struct {
			Sessionid          string
			Expectedgeneration uint64
		}
		if err := decodeRemote(body, &args); err != nil {
			return nil, session.ErrInvalidSession
		}
		id, expected = args.Sessionid, args.Expectedgeneration
	}
	if id == "" {
		return nil, session.ErrInvalidSession
	}
	r.mu.RLock()
	active, revision := r.active, r.revision
	routed, known := r.sessions[id]
	r.mu.RUnlock()
	if known && routed.generation != active && r.archive != nil {
		metadata, err := r.archive(ctx, routed.generation)
		if err != nil {
			return nil, err
		}
		if metadata.Generation != routed.generation {
			return nil, session.ErrOwnerUnavailable
		}
		if metadata.Available {
			return r.stoppedSnapshot(id, expected, revision, metadata.Snapshots)
		}
	}
	if !known {
		image, err := r.inspect(ctx, false)
		if err != nil {
			return nil, err
		}
		if err := r.installIndex(image); err != nil {
			return nil, err
		}
		routed, exists := image.byID[id]
		if !exists {
			return nil, session.ErrSessionUnknown
		}
		if _, live := image.owners[routed.generation]; !live {
			return r.stoppedSnapshot(id, expected, image.revision, image.snapshots)
		}
		return r.forwardFromImage(ctx, op, body, image)
	}
	return r.forwardBySession(ctx, op, body)
}

func (r *Router) stoppedSnapshot(id string, expected, revision uint64, snapshots []session.Snapshot) (json.RawMessage, error) {
	r.mu.RLock()
	current := r.revision
	r.mu.RUnlock()
	if current != revision {
		return nil, session.ErrOwnerUnavailable
	}
	for _, snapshot := range snapshots {
		if snapshot.ID != id {
			continue
		}
		if snapshot.State != session.Closed && snapshot.State != session.Exited {
			return nil, session.ErrOwnerUnavailable
		}
		if expected != 0 && expected != snapshot.Generation {
			return nil, &session.StaleGenerationError{CurrentGeneration: snapshot.Generation}
		}
		return json.Marshal(struct{ V0 session.Snapshot }{snapshot})
	}
	return nil, session.ErrSessionUnknown
}

func (r *Router) forwardFromImage(ctx context.Context, op string, body json.RawMessage, image routerImage) (json.RawMessage, error) {
	id, err := remoteSessionID(body)
	if err != nil {
		return nil, err
	}
	routed, ok := image.byID[id]
	if !ok {
		return nil, session.ErrSessionUnknown
	}
	owner, ok := image.owners[routed.generation]
	if !ok {
		owner, err = r.ensureForGeneration(ctx, routed.generation)
		if err != nil {
			return nil, err
		}
	}
	return owner.Call(ctx, op, body)
}

func (r *Router) sessionImageFor(ctx context.Context, body json.RawMessage) (routerImage, error) {
	id, err := remoteSessionID(body)
	if err != nil {
		return routerImage{}, err
	}
	r.mu.RLock()
	routed, exists := r.sessions[id]
	r.mu.RUnlock()
	if exists {
		owner, err := r.ensureForGeneration(ctx, routed.generation)
		if err != nil {
			return routerImage{}, err
		}
		_, _, revision := r.catalogSnapshot()
		return routerImage{
			revision: revision,
			owners:   map[string]Owner{routed.generation: owner},
			byID:     map[string]routedSession{id: routed},
			counts:   map[string]uint64{},
		}, nil
	}
	image, err := r.inspect(ctx, false)
	if err != nil {
		return routerImage{}, err
	}
	if err := r.installIndex(image); err != nil {
		return routerImage{}, err
	}
	return image, nil
}

func (r *Router) ensureForGeneration(ctx context.Context, generation string) (Owner, error) {
	_, owners, _ := r.catalogSnapshot()
	expected, ok := owners[generation]
	if !ok {
		return Owner{}, session.ErrOwnerUnavailable
	}
	return r.ensureOwner(ctx, generation, expected)
}

// ownerStartFailure means a generation's owner could not be reached or
// started. Unlike identity or source mismatches it carries no trust decision.
type ownerStartFailure struct{ cause error }

func (e ownerStartFailure) Error() string { return e.cause.Error() }
func (e ownerStartFailure) Unwrap() error { return e.cause }

func (r *Router) ensureOwner(ctx context.Context, generation string, expected Owner) (Owner, error) {
	ctx, cancel := context.WithTimeout(ctx, ownerInspectionTimeout)
	defer cancel()
	r.mu.RLock()
	retryAt := r.unavailable[generation]
	r.mu.RUnlock()
	if time.Now().Before(retryAt) {
		return Owner{}, ownerStartFailure{cause: session.ErrOwnerUnavailable}
	}
	owner, err := r.ensure(ctx, generation)
	if err != nil {
		return Owner{}, ownerStartFailure{cause: err}
	}
	if owner.Call == nil || !validOwnerIdentity(generation, owner.Descriptor, owner.Identity) {
		return Owner{}, session.ErrOwnerUnavailable
	}
	if owner.Descriptor == expected.Descriptor {
		if !identityEmpty(expected.Identity) && owner.Identity != expected.Identity {
			return Owner{}, session.ErrOwnerUnavailable
		}
		return owner, nil
	}
	if !identityEmpty(expected.Identity) && !validOwnerIdentity(generation, expected.Descriptor, expected.Identity) {
		return Owner{}, session.ErrOwnerUnavailable
	}
	active, owners, _ := r.catalogSnapshot()
	activeOwner, exists := owners[active]
	if !exists || !currentSourceTransition(expected.Descriptor, owner.Descriptor, activeOwner.Descriptor) {
		return Owner{}, session.ErrOwnerUnavailable
	}
	return owner, nil
}

func currentSourceTransition(expected, actual, active Descriptor) bool {
	expectedSource, expectedExecutable := expected.Source, expected.Executable
	expected.Source = actual.Source
	expected.Executable = actual.Executable
	return expected == actual && actual.Source == active.Source && actual.Executable == active.Executable && (expectedSource != actual.Source || expectedExecutable != actual.Executable)
}

func (r *Router) catalogSnapshot() (string, map[string]Owner, uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	owners := make(map[string]Owner, len(r.owners))
	for generation, owner := range r.owners {
		owners[generation] = owner
	}
	return r.active, owners, r.revision
}

func validOwnerIdentity(generation string, descriptor Descriptor, identity workloadbridge.OwnerIdentity) bool {
	return !identity.Retired && identity.Schema == Schema && identity.Generation == generation && identity.ExecutableSHA256 == descriptor.Source.SHA256
}

func identityEmpty(identity workloadbridge.OwnerIdentity) bool {
	return identity == (workloadbridge.OwnerIdentity{})
}

func remoteSessionID(body json.RawMessage) (string, error) {
	var args struct {
		Sessionid string
	}
	if err := json.Unmarshal(body, &args); err != nil || args.Sessionid == "" {
		return "", session.ErrInvalidSession
	}
	return args.Sessionid, nil
}

func decodeRemote(body []byte, out any) error {
	if len(body) > 8<<20 {
		return session.ErrOwnerUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return session.ErrInvalidSession
	}
	return nil
}

func sortedOwnerKeys(owners map[string]Owner) []string {
	keys := make([]string, 0, len(owners))
	for generation := range owners {
		keys = append(keys, generation)
	}
	sort.Strings(keys)
	return keys
}

func nameExists(snapshots []session.Snapshot, name string) bool {
	for _, snapshot := range snapshots {
		if snapshot.Name == name {
			return true
		}
	}
	return false
}

func isDelete(op string) bool { return op == "Delete" || op == "DeleteAtGeneration" }

// atCeiling is the one check of the machine's terminal ceiling: live sessions across every owner
// generation. It protects the machine from runaway creation; it is not a plan limit. Attachments are
// bounded per session by the session manager.
func (r *Router) atCeiling(image routerImage) bool { return liveSessions(image.byID) >= r.maxIDs }

// maxRetainedRecordsPerOwner bounds one owner's list, live and closed records together, so a
// malformed or runaway owner cannot grow the catalog without limit.
const maxRetainedRecordsPerOwner = 1024
