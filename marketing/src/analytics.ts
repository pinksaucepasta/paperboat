/**
 * Product analytics (PostHog), explicit events, no SDK, no autocapture, no replay, no cookies: PostHog's
 * standard `$pageview` and `$pageleave` (so its Web Analytics shows sessions, bounce, duration, entry and exit
 * pages, referrers, campaigns, browser, OS, device and country), `install_command_copied` and `signup_clicked`.
 * Every event carries `app: "site"`, the path only (no query string, no full address), the referrer's domain,
 * the page's utm_* values, and the coarse browser, OS and device class. The visitor is a random id in
 * sessionStorage (gone when the tab closes) and is also the session id. PostHog derives the country from the
 * request address and discards the address (project setting). The key and host are build settings
 * (PUBLIC_POSTHOG_KEY, PUBLIC_POSTHOG_HOST); without them, with Do Not Track or Global Privacy Control, nothing
 * is sent. Never an email, anything typed or any other page content.
 */
const apiKey = import.meta.env.PUBLIC_POSTHOG_KEY
const host = import.meta.env.PUBLIC_POSTHOG_HOST

const nav = navigator as Navigator & { globalPrivacyControl?: boolean; userAgentData?: { mobile?: boolean } }
const enabled = Boolean(apiKey && host) && nav.doNotTrack !== "1" && !nav.globalPrivacyControl

/** A UUIDv7 (millisecond timestamp first): PostHog's sessions need the session id to be one. */
function uuidv7() {
  const b = crypto.getRandomValues(new Uint8Array(16))
  let t = Date.now()
  for (let i = 5; i >= 0; i--) {
    b[i] = t % 256
    t = Math.floor(t / 256)
  }
  b[6] = (b[6] & 0x0f) | 0x70
  b[8] = (b[8] & 0x3f) | 0x80
  const h = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("")
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`
}

function visitor() {
  try {
    const stored = sessionStorage.getItem("pb-visit")
    if (stored) return stored
    const made = uuidv7()
    sessionStorage.setItem("pb-visit", made)
    return made
  } catch {
    return uuidv7() // Storage blocked: each page is its own visit.
  }
}

/** Coarse browser, OS and device class in PostHog's names (the SDK-less capture API does not derive them). */
function client(ua: string, mobile?: boolean) {
  const browser = /Edg\//.test(ua) ? "Microsoft Edge" : /OPR\/|Opera/.test(ua) ? "Opera" : /Firefox\/|FxiOS/.test(ua) ? "Firefox" : /Chrome\/|CriOS/.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "Other"
  const os = /Windows/.test(ua) ? "Windows" : /Android/.test(ua) ? "Android" : /iPhone|iPad|iPod/.test(ua) ? "iOS" : /Mac OS X|Macintosh/.test(ua) ? "Mac OS X" : /CrOS/.test(ua) ? "Chrome OS" : /Linux|X11/.test(ua) ? "Linux" : "Other"
  const tablet = /iPad/.test(ua) || (/Android/.test(ua) && !/Mobile/.test(ua))
  return { $browser: browser, $os: os, $device_type: tablet ? "Tablet" : mobile || /Mobi|iPhone|iPod/.test(ua) ? "Mobile" : "Desktop" }
}

function utm() {
  const out: Record<string, string> = {}
  const query = new URLSearchParams(location.search)
  for (const key of ["utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content"]) {
    const value = query.get(key)
    if (value) out[key] = value.slice(0, 100)
  }
  return out
}

function referrerHost() {
  try {
    return new URL(document.referrer).host
  } catch {
    return ""
  }
}

function send(event: string, properties: Record<string, string>) {
  if (!enabled) return
  try {
    const id = visitor()
    void fetch(new URL("/i/v0/e/", host), {
      method: "POST",
      keepalive: true, // a click or a page leave still delivers
      headers: { "content-type": "text/plain" }, // no CORS preflight, as PostHog's own SDK sends it
      body: JSON.stringify({
        api_key: apiKey,
        event,
        distinct_id: id,
        properties: {
          app: "site",
          $process_person_profile: false, // anonymous visitors get no person profile

          $session_id: id,
          $host: location.host,
          $pathname: location.pathname,
          $current_url: location.origin + location.pathname,
          ...client(navigator.userAgent, nav.userAgentData?.mobile),
          ...properties,
        },
      }),
    }).catch(() => {})
  } catch {
    // Analytics never breaks the page.
  }
}

export function startAnalytics(consoleOrigin: string) {
  const referrer = referrerHost()
  send("$pageview", {
    $referring_domain: referrer && referrer !== location.host ? referrer : "$direct",
    ...(referrer && { referrer_host: referrer }),
    ...utm(),
  })
  window.addEventListener("pagehide", () => send("$pageleave", {}))
  document.addEventListener("click", (event) => {
    const target = event.target as Element
    const copy = target.closest<HTMLElement>("[data-copy][data-os]")
    if (copy) return send("install_command_copied", { platform: copy.dataset.os ?? "", path: location.pathname })
    const link = target.closest<HTMLAnchorElement>("a[href]")
    if (link && link.origin === consoleOrigin) {
      send("signup_clicked", { label: link.textContent?.trim().slice(0, 40) ?? "", path: location.pathname })
    }
  })
}
