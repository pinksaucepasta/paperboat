/**
 * Product analytics (PostHog): three explicit events, no SDK, no autocapture, no replay, no cookies.
 * `pageview` (path only), `install_command_copied` and `signup_clicked` (a link to the console).
 * The key and host are build settings (PUBLIC_POSTHOG_KEY, PUBLIC_POSTHOG_HOST); without them, with Do Not
 * Track or Global Privacy Control, nothing is sent. A visitor is a random id that lives in sessionStorage
 * (gone when the tab closes); never an email, query string, full URL or anything typed.
 */
const apiKey = import.meta.env.PUBLIC_POSTHOG_KEY
const host = import.meta.env.PUBLIC_POSTHOG_HOST

const nav = navigator as Navigator & { globalPrivacyControl?: boolean }
const enabled = Boolean(apiKey && host) && nav.doNotTrack !== "1" && !nav.globalPrivacyControl

function visitor() {
  try {
    const stored = sessionStorage.getItem("pb-visit")
    if (stored) return stored
    const made = crypto.randomUUID()
    sessionStorage.setItem("pb-visit", made)
    return made
  } catch {
    return crypto.randomUUID() // Storage blocked: each page is its own visit.
  }
}

export function track(event: string, properties: Record<string, string> = {}) {
  if (!enabled) return
  try {
    void fetch(new URL("/i/v0/e/", host), {
      method: "POST",
      keepalive: true, // a click that leaves the page still delivers
      headers: { "content-type": "text/plain" }, // no CORS preflight, as PostHog's own SDK sends it
      body: JSON.stringify({ api_key: apiKey, event, distinct_id: visitor(), properties }),
    }).catch(() => {})
  } catch {
    // Analytics never breaks the page.
  }
}

export function startAnalytics(consoleOrigin: string) {
  track("pageview", { path: location.pathname, ...(referrerHost() && { referrer_host: referrerHost() }) })
  document.addEventListener("click", (event) => {
    const target = event.target as Element
    const copy = target.closest<HTMLElement>("[data-copy][data-os]")
    if (copy) return track("install_command_copied", { platform: copy.dataset.os ?? "", path: location.pathname })
    const link = target.closest<HTMLAnchorElement>("a[href]")
    if (link && link.origin === consoleOrigin) {
      track("signup_clicked", { label: link.textContent?.trim().slice(0, 40) ?? "", path: location.pathname })
    }
  })
}

function referrerHost() {
  try {
    return new URL(document.referrer).host
  } catch {
    return ""
  }
}
