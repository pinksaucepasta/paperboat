package customernotices

import (
	"fmt"
	"html"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
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

// render validates the notice of in.Kind and writes its subject and HTML. late says a breach notice goes out more than
// BreachHours after the operator became aware; deadline is that moment.
func render(in Input, now time.Time) (subject, body string, late bool, deadline time.Time, err error) {
	e := html.EscapeString
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
		body = fmt.Sprintf("<p>Paperboat's prices change from your next renewal on or after %s.</p><p>%s</p><p>This email is our notice, given at least %d days ahead. If you do not agree, cancel in the console under Billing before your renewal; cancelling stops the next renewal.</p>", day.Format("2 Jan 2006"), e(p[0]), NoticeDays)
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
		body = fmt.Sprintf("<p>Our terms change on %s.</p><p>%s</p>", day.Format("2 Jan 2006"), e(p[0]))
		if p[1] != "" {
			body += "<p>We are changing them sooner than " + fmt.Sprint(NoticeDays) + " days because " + e(p[1]) + ".</p>"
		}
		body += fmt.Sprintf("<p>Read the new text: <a href=\"%s\">%s</a></p><p>If you do not agree, cancel and stop using the service before then. A payment you already made for the current period is not affected.</p>", e(p[5]), e(p[5]))
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
		body = fmt.Sprintf("<p>From %s we will use <b>%s</b> to %s, in %s, to process personal data.</p><p>You may object within %d days, on reasonable data protection grounds, by writing to %s. If we cannot resolve the objection you may end the affected service and we refund the unused part of any period you paid in advance.</p>", day.Format("2 Jan 2006"), e(p[0]), e(p[1]), e(p[2]), NoticeDays, e(p[3]))
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
		body = fmt.Sprintf("<p>We became aware on %s (UTC) of a personal data breach that affects your data. We are telling you so you can meet your own duty to notify within 72 hours.</p><p><b>What happened:</b> %s</p><p><b>Data involved:</b> %s</p><p><b>What we are doing:</b> %s</p><p><b>Who to contact:</b> %s</p>", aware.UTC().Format("2 Jan 2006 15:04"), e(p[0]), e(p[1]), e(p[2]), e(p[3]))
	default:
		return "", "", false, time.Time{}, bad("kind is price_increase, terms_change, subprocessor or breach")
	}
	if p[5] != "" && in.Kind != "terms_change" {
		body += fmt.Sprintf("<p>More: <a href=\"%s\">%s</a></p>", e(p[5]), e(p[5]))
	}
	return subject, body, late, deadline, nil
}
