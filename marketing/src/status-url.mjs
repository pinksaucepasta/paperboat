/**
 * Where the status page is: PAPERBOAT_STATUS_URL when set, otherwise `status.` on the site's own domain (the status page's custom
 * domain), only for a real https site so local builds link nowhere. No domain is written in code.
 */
export function statusUrl(siteUrl, override) {
  if (override) return override
  if (!siteUrl) return undefined
  const site = new URL(siteUrl)
  return site.protocol === "https:" ? `https://status.${site.hostname.replace(/^www\./, "")}` : undefined
}
