package usermachines

import (
	"context"

	"github.com/pinksaucepasta/paperboat/server/internal/helperruntime"
)

// SessionParticipant is a live attachment of a terminal session with who it is. The host knows only
// account and client identifiers; the names are resolved here and shown only to the session owner
// and to the participant themselves.
type SessionParticipant struct {
	helperruntime.Participant
	// Kind is "member" for a signed-in account and "guest" for someone who joined with the code.
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
}

// participantKinds names what each attached identity is. A guest is identified to the host by its
// guest session id, which is not an account.
const participantIdentitySQL = `
SELECT ids.id,
       CASE WHEN g.id IS NOT NULL THEN 'guest' ELSE 'member' END,
       coalesce(g.display_name, u.display_name, ''),
       CASE WHEN g.id IS NOT NULL THEN ''
            WHEN u.id IS NULL THEN ''
            WHEN ids.id = $2 THEN u.primary_email
            WHEN EXISTS (SELECT 1 FROM team_members viewer JOIN team_members other ON other.team_id = viewer.team_id
                         WHERE viewer.account_id = $2 AND viewer.active AND other.account_id = ids.id AND other.active) THEN u.primary_email
            WHEN $2 = $4 AND u.email_verified AND EXISTS (
                 SELECT 1 FROM terminal_browser_access_policies p
                 WHERE p.terminal_session_id = $3 AND p.audience = 'specific_emails'
                   AND (lower(u.primary_email) = ANY (p.allowed_emails) OR ('*@' || split_part(lower(u.primary_email), '@', 2)) = ANY (p.allowed_emails))) THEN u.primary_email
            ELSE '' END
FROM unnest($1::text[]) AS ids(id)
LEFT JOIN terminal_guest_sessions g ON g.id = ids.id AND g.terminal_session_id = $3
LEFT JOIN users u ON u.id = ids.id AND g.id IS NULL`

// describeParticipants resolves who is attached. Names and emails go only to the session owner and
// to the participant themselves; an email additionally needs a reason the viewer may already know it
// (their own, a shared team, or an address the owner allow-listed for this terminal).
func (s *Service) describeParticipants(ctx context.Context, viewer, owner, sessionID string, live []helperruntime.Participant) ([]SessionParticipant, error) {
	out := make([]SessionParticipant, 0, len(live))
	if len(live) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(live))
	for _, participant := range live {
		ids = append(ids, participant.AccountID)
	}
	type identity struct{ kind, name, email string }
	known := make(map[string]identity, len(ids))
	rows, err := s.db.Pool().Query(ctx, participantIdentitySQL, ids, viewer, sessionID, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var item identity
		if err = rows.Scan(&id, &item.kind, &item.name, &item.email); err != nil {
			return nil, err
		}
		known[id] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, participant := range live {
		item := known[participant.AccountID]
		entry := SessionParticipant{Participant: participant, Kind: item.kind}
		if entry.Kind == "" {
			entry.Kind = "member"
		}
		if viewer == owner || participant.AccountID == viewer {
			entry.DisplayName, entry.Email = item.name, item.email
		}
		out = append(out, entry)
	}
	return out, nil
}
