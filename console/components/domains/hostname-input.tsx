"use client"

import { CaretDownIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useApiQuery, useWorkspace } from "@/lib/api"
import { usableDomains, workspaceDomainsQuery } from "@/lib/workspace-domains"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { InputGroup, InputGroupAddon, InputGroupInput, InputGroupText } from "@/components/ui/input-group"

/** Splits a hostname into the name typed and the workspace domain it sits under. */
export function splitHostname(hostname: string, domains: string[]) {
  const domain = [...domains]
    .sort((a, b) => b.length - a.length)
    .find((d) => hostname === d || hostname.endsWith(`.${d}`))
  return domain
    ? { label: hostname.slice(0, -domain.length).replace(/\.$/, ""), domain }
    : { label: "", domain: domains[0] }
}

/**
 * A hostname under one of the workspace's verified domains: a name (`app`, `*` for every name, or
 * empty for the domain itself) and the domain. Its DNS and certificate come from the domain, so
 * there is nothing more to set up. `onChange` always gets the whole hostname.
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
  const workspace = useWorkspace()
  const list = useApiQuery({ ...workspaceDomainsQuery(workspace?.id ?? ""), enabled: Boolean(workspace) })
  const domains = usableDomains(list.data)
  if (!list.data)
    return (
      <InputGroup>
        <InputGroupInput disabled aria-label="Hostname" />
      </InputGroup>
    )
  if (!domains.length)
    return (
      <p className="text-sm text-muted-foreground">
        No verified domain yet.{" "}
        <Link href="/domains" className="text-foreground underline underline-offset-4">
          Add a domain
        </Link>{" "}
        first, then pick a name under it here.
      </p>
    )
  const { label, domain } = splitHostname(value, domains)
  const join = (nextLabel: string, nextDomain: string) =>
    onChange(nextLabel.trim().toLowerCase() ? `${nextLabel.trim().toLowerCase()}.${nextDomain}` : nextDomain)
  return (
    <InputGroup>
      <InputGroupInput
        autoFocus={autoFocus}
        aria-label="Name"
        placeholder={placeholder}
        value={label}
        onChange={(event) => join(event.target.value, domain)}
        aria-invalid={invalid}
        className="font-mono"
        autoComplete="off"
        spellCheck={false}
      />
      <InputGroupAddon align="inline-end">
        {domains.length === 1 ? (
          <InputGroupText className="font-mono text-sm">.{domain}</InputGroupText>
        ) : (
          <DropdownMenu>
            <DropdownMenuTrigger
              aria-label="Domain"
              className="inline-flex items-center gap-1 rounded-sm font-mono text-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30"
            >
              .{domain}
              <CaretDownIcon className="size-3.5" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuRadioGroup value={domain} onValueChange={(next) => join(label, String(next))}>
                {domains.map((d) => (
                  <DropdownMenuRadioItem key={d} value={d} className="font-mono">
                    {d}
                  </DropdownMenuRadioItem>
                ))}
              </DropdownMenuRadioGroup>
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </InputGroupAddon>
    </InputGroup>
  )
}
