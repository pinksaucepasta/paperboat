package preview

import (
	"context"
	"errors"
	"sync"
)

// Close releases every source-owned active carrier. It is idempotent and does
// not use a process-global background context; callers supply the shutdown
// deadline that bounds transport cleanup.
func (s *MachineAttachmentSessionSource) Close(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrMachineAttachmentSessionInvalid
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	entries := make([]*machineAttachmentSessionEntry, 0, len(s.allEntries))
	for entry := range s.allEntries {
		entries = append(entries, entry)
	}
	s.entries = make(map[machineAttachmentSessionKey]*machineAttachmentSessionEntry)
	s.allEntries = make(map[*machineAttachmentSessionEntry]struct{})
	s.mu.Unlock()
	var result error
	for _, entry := range entries {
		if entry != nil && entry.active != nil {
			result = errors.Join(result, entry.active.Close(ctx))
		}
	}
	return result
}

func (s *MachineAttachmentSessionSource) sessionForEntry(key machineAttachmentSessionKey, entry *machineAttachmentSessionEntry) AttachmentSession {
	var once sync.Once
	var releaseErr error
	return AttachmentSession{
		Active: entry.active, Identity: entry.identity,
		Release: func(ctx context.Context) error {
			once.Do(func() { releaseErr = s.release(key, entry, ctx) })
			return releaseErr
		},
	}
}

func (s *MachineAttachmentSessionSource) release(key machineAttachmentSessionKey, entry *machineAttachmentSessionEntry, ctx context.Context) error {
	if ctx == nil {
		return ErrMachineAttachmentSessionInvalid
	}
	s.mu.Lock()
	current := s.entries[key]
	if current != entry {
		s.mu.Unlock()
		return nil
	}
	if current.refs > 0 {
		current.refs--
	}
	if current.refs != 0 {
		s.mu.Unlock()
		return nil
	}
	delete(s.entries, key)
	s.mu.Unlock()
	return s.retire(current, ctx)
}

// retire fences new admission immediately. Existing work owns the remaining
// lifetime, with no retirement deadline. allEntries retains revocation ownership
// until the final stream finishes and the carrier is reclaimed.
func (s *MachineAttachmentSessionSource) retire(entry *machineAttachmentSessionEntry, ctx context.Context) error {
	if entry == nil || entry.active == nil {
		return nil
	}
	s.mu.Lock()
	if entry.retiring {
		s.mu.Unlock()
		return nil
	}
	entry.retiring = true
	s.mu.Unlock()
	if err := entry.active.BeginDrain(); err != nil {
		return err
	}
	finish := func() error {
		err := entry.active.Drain(context.WithoutCancel(ctx))
		if err == nil {
			s.mu.Lock()
			delete(s.allEntries, entry)
			s.mu.Unlock()
		}
		return err
	}
	if entry.active.ActiveStreams() == 0 {
		return finish()
	}
	go func() {
		if err := finish(); err != nil {
			logAttachmentFailure(context.WithoutCancel(ctx), "carrier_retirement", err)
		}
	}()
	return nil
}
