package transactionalmail

import (
	"html"
	"net/url"
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
	// Brand colours (brand/styles/brand.css): ground, paper, ink, text, fog, rule and the cobalt button; the dark button
	// is the lightest blue that keeps white text at 4.5:1 or better.
	darkStyle = `@media (prefers-color-scheme: dark){.bg{background:#141414!important}.card{background:#1c1c1c!important;border-color:#2e2e2e!important}` +
		`.ink{color:#f4f4f2!important}.txt{color:#dcdcd8!important}.fog{color:#a3a3a0!important}.btn{background:#3868f0!important}}`
)

// Compose renders c with the shared layout and returns the message with its HTML and plain-text bodies. The wordmark,
// logo and footer come from the deployment's mail configuration.
func Compose(policy config.TransactionalMail, to, subject string, c Content) Message {
	site, logo := strings.TrimRight(policy.ConsoleOrigin, "/"), ""
	if policy.SiteURL != "" {
		site = strings.TrimRight(policy.SiteURL, "/")
		logo = site + "/apple-touch-icon.png"
	}
	host := site
	if u, err := url.Parse(site); err == nil && u.Host != "" {
		host = u.Host
	}
	footer := "Paperboat · " + host
	return Message{From: policy.From, To: []string{to}, Subject: subject, HTML: renderHTML(logo, site, footer, subject, c), Text: renderText(footer, c)}
}

func renderHTML(logo, site, footer, title string, c Content) string {
	e := html.EscapeString
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<meta name="color-scheme" content="light dark"><meta name="supported-color-schemes" content="light dark"><title>` + e(title) + `</title><style>` + darkStyle + `</style></head>` +
		`<body class="bg" style="margin:0;padding:0;background:#fafaf9"><table role="presentation" class="bg" width="100%" cellpadding="0" cellspacing="0" style="background:#fafaf9">` +
		`<tr><td align="center" style="padding:32px 16px"><table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px">` +
		`<tr><td style="padding:0 0 20px;font-family:` + fontStack + `;font-size:20px;font-weight:600;letter-spacing:-0.025em">`)
	if logo != "" {
		b.WriteString(`<img src="` + e(logo) + `" width="28" height="28" alt="Paperboat" style="vertical-align:middle;border:0;border-radius:6px;margin-right:8px">`)
	}
	b.WriteString(`<a href="` + e(site) + `" class="ink" style="vertical-align:middle;color:#141414;text-decoration:none">paperboat</a></td></tr>` +
		`<tr><td class="card" style="background:#ffffff;border:1px solid #e8e8e6;border-radius:12px;padding:32px;font-family:` + fontStack + `;overflow-wrap:anywhere;word-break:break-word">`)
	if c.Heading != "" {
		b.WriteString(`<h1 class="ink" style="margin:0 0 16px;font-size:20px;line-height:1.3;font-weight:600;color:#141414">` + e(c.Heading) + `</h1>`)
	}
	for _, p := range c.Paragraphs {
		b.WriteString(`<p class="txt" style="margin:0 0 16px;font-size:15px;line-height:1.6;color:#2a2a2a">` + e(p) + `</p>`)
	}
	for _, d := range c.Details {
		b.WriteString(`<p class="txt" style="margin:0 0 16px;font-size:15px;line-height:1.6;color:#2a2a2a"><span class="fog" style="display:block;font-size:12px;color:#6b6b6b">` + e(d.Label) +
			`</span><span style="white-space:pre-wrap">` + e(d.Value) + `</span></p>`)
	}
	if c.Button != nil {
		b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" style="margin:8px 0 0"><tr><td class="btn" style="background:#1d52eb;border-radius:8px">` +
			`<a href="` + e(c.Button.URL) + `" style="display:inline-block;padding:12px 20px;font-size:15px;font-weight:600;color:#ffffff;text-decoration:none">` + e(c.Button.Label) + `</a></td></tr></table>`)
	}
	b.WriteString(`</td></tr><tr><td class="fog" style="padding:20px 4px 0;font-family:` + fontStack + `;font-size:12px;line-height:1.6;color:#6b6b6b;overflow-wrap:anywhere;word-break:break-word">` + e(footer))
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
