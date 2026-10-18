/** Helpers for domain names shared by the Domains page and the hostname fields. */

/** Whether `hostname` (or a wildcard of it) is `domain` or under it. */
export const isUnder = (hostname: string, domain: string) => {
  const host = hostname.replace(/^\*\./, "")
  return host === domain || host.endsWith(`.${domain}`)
}

/** A domain as people type it: lowercase, no scheme, no trailing dot. */
export const cleanDomain = (value: string) =>
  value
    .trim()
    .toLowerCase()
    .replace(/^[a-z]+:\/\//, "")
    .replace(/\/.*$/, "")
    .replace(/^\.+|\.+$/g, "")

export const domainPattern = /^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$/
