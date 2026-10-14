package sessionowner

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

// Connections owns only private client connections, never shells or stores.
// Start asks the account's privileged updater to reopen an archived generation.
type Connections struct {
	mu          sync.Mutex
	catalogMu   sync.Mutex
	catalog     Catalog
	active      Descriptor
	clients     map[string]*workloadbridge.Client
	descriptors map[string]Descriptor
	start       func(context.Context, string) error
	router      *Router
}

func NewConnections(ctx context.Context, catalog Catalog, active Descriptor, start func(context.Context, string) error, maxSessions, maxAttachments int) (*Connections, error) {
	if start == nil || active.Validate() != nil {
		return nil, ErrInvalid
	}
	all, err := catalog.ReadAll(ctx, active.Instance)
	if err != nil {
		return nil, err
	}
	owners := make(map[string]Owner, len(all))
	for _, d := range all {
		if !sameAccount(d, active) {
			return nil, ErrInvalid
		}
		owners[d.Generation] = Owner{Descriptor: d}
	}
	connections := &Connections{catalog: catalog, active: active, start: start, clients: make(map[string]*workloadbridge.Client), descriptors: make(map[string]Descriptor)}
	router, err := NewRouter(active.Generation, owners, maxSessions, maxAttachments, connections.ensure)
	if err != nil {
		return nil, err
	}
	connections.router = router
	router.archive = connections.archiveMetadata
	// Installation must prestart the active owner; startup does not depend on
	// updated becoming ready after hostd and the local daemon.
	client, err := Connect(ctx, active)
	if err != nil {
		return nil, err
	}
	connections.clients[active.Generation] = client
	connections.descriptors[active.Generation] = active
	return connections, nil
}

func (c *Connections) archiveMetadata(ctx context.Context, generation string) (workloadbridge.ArchiveMetadata, error) {
	// This authenticated reader runs in the current unprivileged owner. No
	// archived native process needs to start for global admission or listing.
	c.mu.Lock()
	client := c.clients[c.active.Generation]
	c.mu.Unlock()
	if client == nil {
		return workloadbridge.ArchiveMetadata{}, session.ErrOwnerUnavailable
	}
	return client.ReadArchiveMetadata(ctx, generation)
}
func sameAccount(a, b Descriptor) bool {
	return a.Instance == b.Instance && a.RuntimeRoot == b.RuntimeRoot && a.UID == b.UID && a.GID == b.GID && a.OwnerSID == b.OwnerSID
}
func (c *Connections) Service() (*session.RemoteManager, error) {
	if c == nil || c.router == nil {
		return nil, ErrInvalid
	}
	return session.NewRemoteManager(func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		if refreshCatalogOperation(op) {
			if err := c.refreshCatalog(ctx); err != nil {
				return nil, err
			}
		}
		return c.router.Call(ctx, op, body)
	})
}

// refreshCatalog applies protected descriptor changes before operations whose
// routing or admission depends on the complete owner set. Fast per-session
// input and output calls keep using the cached ID index.
func (c *Connections) refreshCatalog(ctx context.Context) error {
	if c == nil || c.router == nil || ctx == nil {
		return ErrInvalid
	}
	c.catalogMu.Lock()
	defer c.catalogMu.Unlock()
	all, err := c.catalog.ReadAll(ctx, c.active.Instance)
	if err != nil {
		return err
	}
	owners := make(map[string]Owner, len(all))
	for _, descriptor := range all {
		if descriptor.Validate() != nil || !sameAccount(descriptor, c.active) {
			return ErrInvalid
		}
		owners[descriptor.Generation] = Owner{Descriptor: descriptor}
	}
	if _, exists := owners[c.active.Generation]; !exists {
		return session.ErrOwnerUnavailable
	}

	active, previous, _ := c.router.catalogSnapshot()
	if active == c.active.Generation && sameOwnerCatalog(previous, owners) {
		return nil
	}
	return c.router.SetOwners(c.active.Generation, owners)
}

func sameOwnerCatalog(a, b map[string]Owner) bool {
	if len(a) != len(b) {
		return false
	}
	for generation, owner := range a {
		other, exists := b[generation]
		if !exists || owner.Descriptor != other.Descriptor {
			return false
		}
	}
	return true
}

func refreshCatalogOperation(op string) bool {
	switch op {
	case "List", "ResourceCounts", "Snapshot", "SnapshotAtGeneration", "Create", "Delete", "DeleteAtGeneration", "Restart", "RestartAtGeneration", "RestartContext", "RestartAtGenerationContext", "Attach", "AttachParticipant", "AttachParticipantAtGeneration", "AttachLive", "AttachLiveParticipant", "AttachLiveParticipantAtGeneration":
		return true
	default:
		return false
	}
}
func (c *Connections) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, client := range c.clients {
		client.Close()
		delete(c.clients, key)
		delete(c.descriptors, key)
	}
}

func (c *Connections) ensure(ctx context.Context, generation string) (Owner, error) {
	return c.resolve(ctx, generation, false)
}
func (c *Connections) resolve(ctx context.Context, generation string, refresh bool) (Owner, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, err := Read(filepath.Join(c.catalog.Root, generation+".json"))
	if err != nil {
		return Owner{}, err
	}
	if d.Generation != generation || !sameAccount(d, c.active) {
		return Owner{}, ErrInvalid
	}
	client := c.clients[generation]
	if refresh || c.descriptors[generation] != d || client == nil {
		client, err = Connect(ctx, d)
	}
	if err != nil {
		if !errors.Is(err, session.ErrOwnerUnavailable) && !errors.Is(err, session.ErrManagerStopped) {
			return Owner{}, err
		}
		if err := c.start(ctx, generation); err != nil {
			return Owner{}, err
		}
		d, err = Read(filepath.Join(c.catalog.Root, generation+".json"))
		if err != nil {
			return Owner{}, err
		}
		if d.Generation != generation || !sameAccount(d, c.active) {
			return Owner{}, ErrInvalid
		}
		client, err = Connect(ctx, d)
		if err != nil {
			return Owner{}, err
		}
	}
	if old := c.clients[generation]; old != nil && old != client {
		old.Close()
	}
	c.clients[generation] = client
	c.descriptors[generation] = d
	owner := connectedOwner(d, client)
	call := owner.Call
	owner.Call = func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		result, err := call(ctx, op, body)
		// Only this explicit pre-dispatch fence proves that the operation did
		// not execute. Transport failures may be ambiguous and are never replayed.
		if err != session.ErrManagerStopped {
			if errors.Is(err, session.ErrOwnerUnavailable) {
				c.mu.Lock()
				if c.clients[generation] == client {
					delete(c.clients, generation)
					delete(c.descriptors, generation)
					client.Close()
				}
				c.mu.Unlock()
			}
			return result, err
		}
		fresh, err := c.resolve(ctx, generation, true)
		if err != nil {
			return nil, err
		}
		// One retry only, through the newly authenticated raw connection.
		c.mu.Lock()
		replacement := c.clients[generation]
		c.mu.Unlock()
		if replacement == nil {
			return nil, session.ErrOwnerUnavailable
		}
		_ = fresh
		return replacement.Call(ctx, "session", op, body)
	}
	return owner, nil
}
