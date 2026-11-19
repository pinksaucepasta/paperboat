package customernotices

import (
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

const (
	// NoticeDays is the advance notice the Terms and Subprocessors pages promise.
	NoticeDays = 30
	// BreachHours is the window the DPA promises for telling customers about a breach.
	BreachHours = 48
	maxField    = 1000
)

// Input is what an operator writes; the fields each kind uses are named in render.
type Input struct {
	Kind          string `json:"kind"`
	AudienceKind  string `json:"audience_kind"`
	AudienceValue string `json:"audience_value"`
	// DryRun renders the notice and counts its recipients without sending anything.
	DryRun       bool   `json:"dry_run"`
	Effective    string `json:"effective,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Link         string `json:"link,omitempty"`
	UrgentReason string `json:"urgent_reason,omitempty"`
	Name         string `json:"name,omitempty"`
	Purpose      string `json:"purpose,omitempty"`
	Location     string `json:"location,omitempty"`
	AwareAt      string `json:"aware_at,omitempty"`
	What         string `json:"what,omitempty"`
	Data         string `json:"data,omitempty"`
	Actions      string `json:"actions,omitempty"`
	Contact      string `json:"contact,omitempty"`
}

func bad(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

func text(name, value string, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" || utf8.RuneCountInString(value) > maxField {
		return "", bad("%s is required and at most %d characters", name, maxField)
	}
	return value, nil
}

// effective parses the date a change takes effect and refuses one closer than the promised notice.
func effective(value string, now time.Time, urgent bool) (time.Time, error) {
	day, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, bad("the effective date is YYYY-MM-DD")
	}
	if !urgent && day.Before(now.UTC().Truncate(24*time.Hour).AddDate(0, 0, NoticeDays)) {
		return time.Time{}, bad("the effective date must be at least %d days away", NoticeDays)
	}
	return day, nil
}

// render validates the notice of in.Kind and writes its subject and content. late says a breach notice goes out more than
// BreachHours after the operator became aware; deadline is that moment.
func render(in Input, now time.Time) (subject string, c transactionalmail.Content, late bool, deadline time.Time, err error) {
	var p [6]string
	get := func(i int, name, value string, required bool) {
		if err == nil {
			p[i], err = text(name, value, required)
		}
	}
	link := func(required bool) {
		if err != nil || (p[5] == "" && !required) {
			return
		}
		if u, perr := url.Parse(p[5]); perr != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			err = bad("the link is an https address")
		}
	}
	get(5, "link", in.Link, false)
	switch in.Kind {
	case "price_increase":
		get(0, "detail", in.Detail, true)
		var day time.Time
		if err == nil {
			day, err = effective(in.Effective, now, false)
		}
		link(false)
		if err != nil {
			return
		}
		subject = "Paperboat: prices change on " + day.Format("2 Jan 2006")
		c.Heading = "Prices change on " + day.Format("2 Jan 2006")
		c.Paragraphs = []string{
			fmt.Sprintf("Paperboat's prices change from your next renewal on or after %s.", day.Format("2 Jan 2006")),
			p[0],
			fmt.Sprintf("This email is our notice, given at least %d days ahead. If you do not agree, cancel in the console under Billing before your renewal; cancelling stops the next renewal.", NoticeDays),
		}
	case "terms_change":
		get(0, "detail", in.Detail, true)
		get(1, "urgent_reason", in.UrgentReason, false)
		var day time.Time
		if err == nil {
			day, err = effective(in.Effective, now, p[1] != "")
		}
		link(true)
		if err != nil {
			return
		}
		subject = "Paperboat: our terms change on " + day.Format("2 Jan 2006")
		c.Heading = "Our terms change on " + day.Format("2 Jan 2006")
		c.Paragraphs = []string{p[0]}
		if p[1] != "" {
			c.Paragraphs = append(c.Paragraphs, fmt.Sprintf("We are changing them sooner than %d days because %s.", NoticeDays, p[1]))
		}
		c.Paragraphs = append(c.Paragraphs, "If you do not agree, cancel and stop using the service before then. A payment you already made for the current period is not affected.")
		c.Button = &transactionalmail.Button{Label: "Read the new terms", URL: p[5]}
	case "subprocessor":
		get(0, "name", in.Name, true)
		get(1, "purpose", in.Purpose, true)
		get(2, "location", in.Location, true)
		get(3, "contact", in.Contact, true)
		var day time.Time
		if err == nil {
			day, err = effective(in.Effective, now, false)
		}
		if a, perr := mail.ParseAddress(p[3]); err == nil && (perr != nil || a.Address != p[3]) {
			err = bad("contact is an email address")
		}
		link(false)
		if err != nil {
			return
		}
		subject = "Paperboat: new subprocessor " + p[0]
		c.Heading = "New subprocessor: " + p[0]
		c.Paragraphs = []string{
			fmt.Sprintf("From %s we will use %s to %s, in %s, to process personal data.", day.Format("2 Jan 2006"), p[0], p[1], p[2]),
			fmt.Sprintf("You may object within %d days, on reasonable data protection grounds, by writing to %s. If we cannot resolve the objection you may end the affected service and we refund the unused part of any period you paid in advance.", NoticeDays, p[3]),
		}
	case "breach":
		get(0, "what", in.What, true)
		get(1, "data", in.Data, true)
		get(2, "actions", in.Actions, true)
		get(3, "contact", in.Contact, true)
		var aware time.Time
		if err == nil {
			if aware, err = time.Parse(time.RFC3339, in.AwareAt); err != nil || aware.After(now) {
				err = bad("aware_at is the time we became aware, RFC 3339, not in the future")
			}
		}
		if a, perr := mail.ParseAddress(p[3]); err == nil && (perr != nil || a.Address != p[3]) {
			err = bad("contact is an email address")
		}
		link(false)
		if err != nil {
			return
		}
		deadline = aware.Add(BreachHours * time.Hour)
		late = now.After(deadline)
		subject = "Paperboat: notice of a personal data breach"
		c.Heading = "Notice of a personal data breach"
		c.Paragraphs = []string{fmt.Sprintf("We became aware on %s (UTC) of a personal data breach that affects your data. We are telling you so you can meet your own duty to notify within 72 hours.", aware.UTC().Format("2 Jan 2006 15:04"))}
		c.Details = []transactionalmail.Detail{{Label: "What happened", Value: p[0]}, {Label: "Data involved", Value: p[1]}, {Label: "What we are doing", Value: p[2]}, {Label: "Who to contact", Value: p[3]}}
	default:
		return "", c, false, time.Time{}, bad("kind is price_increase, terms_change, subprocessor or breach")
	}
	if p[5] != "" && in.Kind != "terms_change" {
		c.Button = &transactionalmail.Button{Label: "Read more", URL: p[5]}
	}
	c.Why = "You got this because you have a Paperboat account. It is a notice we owe our customers."
	return subject, c, late, deadline, nil
}
