package transactionalmail

import (
	"html"
	"net/url"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// Content is one email: a heading, a few plain-text paragraphs, optional label/value rows, at most one button and the
// reason the person got it. Everything is plain text; Compose escapes it, so callers never build HTML.
type Content struct {
	Heading    string
	Paragraphs []string
	Details    []Detail
	Button     *Button
	Why        string
}

type Detail struct{ Label, Value string }

type Button struct{ Label, URL string }

const (
	fontStack = `-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif`
	// Colours are the brand tokens (brand/styles/brand.css) on the console's white and near-black grounds: ink, text and
	// fog for type, rule for the one divider, cobalt for the one button. The button keeps its blue in dark mode, as the
	// console's primary action does; the lockup goes white there (brand guidelines, reversed).
	darkStyle = `@media (prefers-color-scheme: dark){.bg{background:#141414!important}.ink{color:#f4f4f4!important}.txt{color:#e2e2e2!important}` +
		`.fog{color:#9b9b9b!important}.rule{border-color:#2a2a2a!important}.mk{background:#ffffff!important}.wm{font-weight:500!important}}`
	markCell = 2 // px per grid column; rows are twice as tall, as the mark's 5:3 shape needs
)

// markRows is the boat on the brand's 20 x 6 grid (brand/src/brand.ts), one string per row.
var markRows = [6]string{
	".........##.........",
	".......######.......",
	".....##########.....",
	"####............####",
	"..################..",
	"....############....",
}

// Compose renders c with the shared layout and returns the message with its HTML and plain-text bodies. The lockup
// links to the marketing site and the footer names it, both from the deployment's mail configuration.
func Compose(policy config.TransactionalMail, to, subject string, c Content) Message {
	site := strings.TrimRight(policy.ConsoleOrigin, "/")
	if policy.SiteURL != "" {
		site = strings.TrimRight(policy.SiteURL, "/")
	}
	host := site
	if u, err := url.Parse(site); err == nil && u.Host != "" {
		host = u.Host
	}
	footer := "Paperboat · " + host
	return Message{From: policy.From, To: []string{to}, Subject: subject, HTML: renderHTML(site, footer, subject, c), Text: renderText(footer, c)}
}

// lockup is the mark as table cells (an image would be blocked or invisible in dark mode) beside the lowercase wordmark.
func lockup(site string) string {
	var b strings.Builder
	cell := strconv.Itoa(markCell)
	b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" style="border-collapse:collapse"><tr><td style="padding:0 16px 0 0;vertical-align:middle">` +
		`<table role="presentation" cellpadding="0" cellspacing="0" aria-hidden="true" style="border-collapse:collapse;table-layout:fixed;width:` + strconv.Itoa(20*markCell) + `px"><tr>`)
	// The first row fixes the 20 column widths; every later row spans them in runs.
	b.WriteString(strings.Repeat(`<td width="`+cell+`" style="width:`+cell+`px;height:0;padding:0;font-size:0;line-height:0"></td>`, 20) + `</tr>`)
	for _, row := range markRows {
		b.WriteString(`<tr>`)
		for i := 0; i < len(row); {
			j := i
			for j < len(row) && row[j] == row[i] {
				j++
			}
			style := `height:` + strconv.Itoa(2*markCell) + `px;padding:0;font-size:0;line-height:0`
			if row[i] == '#' {
				style += `;background:#1d52eb`
				b.WriteString(`<td colspan="` + strconv.Itoa(j-i) + `" class="mk" style="` + style + `"></td>`)
			} else {
				b.WriteString(`<td colspan="` + strconv.Itoa(j-i) + `" style="` + style + `"></td>`)
			}
			i = j
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</table></td><td style="vertical-align:middle"><a href="` + html.EscapeString(site) + `" class="ink wm" style="font-family:` + fontStack +
		`;font-size:24px;line-height:24px;font-weight:600;letter-spacing:-0.025em;color:#141414;text-decoration:none">paperboat</a></td></tr></table>`)
	return b.String()
}

func renderHTML(site, footer, title string, c Content) string {
	e := html.EscapeString
	const p = `margin:0 0 16px;font-size:15px;line-height:24px;color:#2a2a2a`
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<meta name="color-scheme" content="light dark"><meta name="supported-color-schemes" content="light dark"><title>` + e(title) + `</title><style>` + darkStyle + `</style></head>` +
		`<body class="bg" style="margin:0;padding:0;background:#ffffff"><table role="presentation" class="bg" width="100%" cellpadding="0" cellspacing="0" style="background:#ffffff">` +
		`<tr><td align="center" style="padding:40px 24px"><table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px">` +
		`<tr><td style="padding:0 0 40px">` + lockup(site) + `</td></tr>` +
		`<tr><td style="padding:0 0 40px;font-family:` + fontStack + `;overflow-wrap:anywhere;word-break:break-word">`)
	if c.Heading != "" {
		b.WriteString(`<h1 class="ink" style="margin:0 0 16px;font-size:20px;line-height:28px;font-weight:600;letter-spacing:-0.01em;color:#141414">` + e(c.Heading) + `</h1>`)
	}
	for _, para := range c.Paragraphs {
		b.WriteString(`<p class="txt" style="` + p + `">` + e(para) + `</p>`)
	}
	for _, d := range c.Details {
		b.WriteString(`<p class="txt" style="` + p + `"><span class="fog" style="display:block;font-size:12px;line-height:16px;color:#6b6b6b">` + e(d.Label) +
			`</span><span style="white-space:pre-wrap">` + e(d.Value) + `</span></p>`)
	}
	if c.Button != nil {
		b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" style="margin:24px 0 0"><tr><td style="background:#1d52eb;border-radius:8px">` +
			`<a href="` + e(c.Button.URL) + `" style="display:inline-block;padding:10px 16px;font-size:15px;line-height:20px;font-weight:500;color:#ffffff;text-decoration:none">` + e(c.Button.Label) + `</a></td></tr></table>`)
	}
	b.WriteString(`</td></tr><tr><td class="fog rule" style="border-top:1px solid #e8e8e6;padding:24px 0 0;font-family:` + fontStack + `;font-size:12px;line-height:20px;color:#6b6b6b;overflow-wrap:anywhere;word-break:break-word">` + e(footer))
	if c.Why != "" {
		b.WriteString(`<br>` + e(c.Why))
	}
	b.WriteString(`</td></tr></table></td></tr></table></body></html>`)
	return b.String()
}

func renderText(footer string, c Content) string {
	var parts []string
	if c.Heading != "" {
		parts = append(parts, c.Heading)
	}
	parts = append(parts, c.Paragraphs...)
	for _, d := range c.Details {
		parts = append(parts, d.Label+": "+d.Value)
	}
	if c.Button != nil {
		parts = append(parts, c.Button.Label+": "+c.Button.URL)
	}
	tail := footer
	if c.Why != "" {
		tail += "\n" + c.Why
	}
	return strings.Join(parts, "\n\n") + "\n\n-- \n" + tail + "\n"
}
