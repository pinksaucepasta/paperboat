/**
 * Where the status page is: PAPERBOAT_STATUS_URL when set, otherwise `status.` on the site's own domain
 * (PAPERBOAT_SITE_URL), only for a real https site. Same rule as marketing/src/status-url.mjs; no domain is written in code.
 */
export function statusUrl(siteUrl: string | undefined, override: string | undefined): string | undefined {
  if (override) return override
  if (!siteUrl) return undefined
  const site = new URL(siteUrl)
  return site.protocol === "https:" ? `https://status.${site.hostname.replace(/^www\./, "")}` : undefined
}
