package testfixtures

import (
	"context"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"net/netip"
	"regexp"
	"strings"
)

// Comment marks every record this service creates; it only ever reads or deletes records that carry it.
const Comment = "paperboat-test-fixture"

var (
	labelPattern    = regexp.MustCompile(`^_?[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	recordIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type Record struct {
	ID      string
	Type    string
	Name    string
	Content string
	Comment string
}

// Zone is the confined test domain.
func (s *Service) Zone() (string, error) {
	if s.cfg.Domain == "" || s.cfg.ZoneID == "" || s.cfg.DNS == nil {
		return "", ErrNotConfigured
	}
	return s.cfg.Domain, nil
}

func (s *Service) underDomain(name string) bool {
	prefix, ok := strings.CutSuffix(name, "."+s.cfg.Domain)
	if !ok || prefix == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(prefix, ".") {
		if !labelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func validContent(typ, content string) bool {
	switch typ {
	case "CNAME":
		if len(content) > 253 || content == "" {
			return false
		}
		if _, err := netip.ParseAddr(content); err == nil {
			return false
		}
		for _, label := range strings.Split(strings.TrimSuffix(strings.ToLower(content), "."), ".") {
			if !labelPattern.MatchString(label) {
				return false
			}
		}
		return true
	case "TXT":
		if content == "" || len(content) > 255 {
			return false
		}
		for _, c := range content {
			if c < 0x20 || c > 0x7e {
				return false
			}
		}
		return true
	case "A":
		ip, err := netip.ParseAddr(content)
		return err == nil && ip.Is4() && ip.IsGlobalUnicast() && !ip.IsPrivate()
	}
	return false
}

// CreateRecord writes one record (ttl 60, not proxied) named under the test domain.
func (s *Service) CreateRecord(ctx context.Context, typ, name, content string) (Record, error) {
	if _, err := s.Zone(); err != nil {
		return Record{}, err
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if !s.underDomain(name) || !validContent(typ, content) {
		return Record{}, fmt.Errorf("%w: type %q name %q must be a CNAME, TXT or A record under %s", ErrInvalid, typ, name, s.cfg.Domain)
	}
	record := Record{Type: typ, Name: name, Content: content, Comment: Comment}
	id, err := s.cfg.DNS.Create(ctx, s.cfg.ZoneID, record)
	if err != nil {
		return Record{}, err
	}
	record.ID = id
	return record, s.auditDNS(ctx, "test_fixture_dns_created", name, map[string]any{"type": typ, "record_id": id})
}

// own returns the record when it exists, was created by this service and is under the test domain.
func (s *Service) own(ctx context.Context, id string) (Record, error) {
	if _, err := s.Zone(); err != nil {
		return Record{}, err
	}
	if !recordIDPattern.MatchString(id) {
		return Record{}, ErrInvalid
	}
	record, found, err := s.cfg.DNS.Get(ctx, s.cfg.ZoneID, id)
	if err != nil {
		return Record{}, err
	}
	if !found || record.Comment != Comment || !s.underDomain(strings.ToLower(record.Name)) {
		return Record{}, ErrRecordNotFound
	}
	record.ID = id
	return record, nil
}

func (s *Service) Record(ctx context.Context, id string) (Record, error) { return s.own(ctx, id) }

// DeleteRecord removes a record this service created; an already removed or foreign id is "not found".
func (s *Service) DeleteRecord(ctx context.Context, id string) error {
	record, err := s.own(ctx, id)
	if err != nil {
		return err
	}
	if err := s.cfg.DNS.Delete(ctx, s.cfg.ZoneID, id); err != nil {
		return err
	}
	return s.auditDNS(ctx, "test_fixture_dns_deleted", record.Name, map[string]any{"type": record.Type, "record_id": id})
}

func (s *Service) auditDNS(ctx context.Context, event, name string, meta map[string]any) error {
	meta["name"] = name
	return s.cfg.Audit.Write(ctx, audit.Event{ActorType: audit.ActorSystem, EventType: event, ResourceType: "dns_record", ResourceID: name, Metadata: meta})
}
