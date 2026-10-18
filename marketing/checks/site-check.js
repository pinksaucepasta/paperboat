// Browser check for S2-01. Set window.PAPERBOAT_API_URL, then paste into the console of the marketing site (dev or built). Compares the
// page with the server's client configuration (the same response the site is built from) and PLAN 1.1/1.3.
(async () => {
  const api = window.PAPERBOAT_API_URL // set it first: window.PAPERBOAT_API_URL = "<api origin>"
  const config = (await (await fetch(`${api}/v1/client-configuration`)).json()).data
  const text = document.body.innerText
  const copy = [...document.querySelectorAll("[data-copy]")].map((el) => el.dataset.copy)
  const checks = {
    installUsesConfiguredHost: copy.includes(`curl -fsSL ${config.installer_base_url}/install | bash`),
    noWindows: !/windows|powershell/i.test(document.documentElement.outerHTML),
    plans: ["Free", "Pro", "Team"].every((n) => text.includes(n)) &&
      ["$0", "$8", "$80/year", "$15", "$150/seat/year", "5 GB", "100 GB"].every((t) => text.includes(t)),
    directIsFree: /Direct peer-to-peer traffic is always free/.test(text),
    signInUsesConsoleUrl: [...document.querySelectorAll("a")].some((a) => a.href.replace(/\/$/, "") === config.console_url.replace(/\/$/, "")),
    domainsFromConfig: text.includes(`.${config.local_access_base_domain}`) && text.includes(`.${config.preview_base_domain}`),
    noRemovedDomains: !/\.local\.pprbt\.dev|preview\.pprbt\.dev|tunnels\.pprbt\.dev/.test(document.documentElement.outerHTML),
  }
  return checks
})()
