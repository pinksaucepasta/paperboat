"use client"

import { Input } from "@/components/ui/input"

/**
 * A full hostname on a domain you control (`app.acme.dev`, or `*.acme.dev` for every subdomain).
 * Ownership is checked where the hostname is used: a tunnel hostname gets its DNS record and
 * verification on the tunnel. `onChange` always gets the whole hostname.
 */
export function HostnameInput({
  value,
  onChange,
  invalid,
  autoFocus,
  placeholder = "app",
}: {
  value: string
  onChange: (hostname: string) => void
  invalid?: boolean
  autoFocus?: boolean
  placeholder?: string
}) {
  return (
    <Input
      autoFocus={autoFocus}
      aria-label="Hostname"
      placeholder={`${placeholder}.example.com`}
      value={value}
      onChange={(event) => onChange(event.target.value.trim().toLowerCase())}
      aria-invalid={invalid}
      className="font-mono"
      autoComplete="off"
      spellCheck={false}
    />
  )
}
