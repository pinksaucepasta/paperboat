"use client"

import { CaretDownIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useState } from "react"
import { isUnder, useVerifiedDomains } from "@/lib/domains"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"

/**
 * A hostname on one of your domains, Cloudflare-style: a subdomain and a domain picked from the
 * ones verified on the Domains page (`app` + `acme.dev`; `*` for every subdomain; empty for the
 * domain itself). Without verified domains it takes a full hostname. `onChange` always gets the
 * whole hostname.
 */
export function HostnameInput({
  value,
  onChange,
  invalid,
  autoFocus,
  placeholder = "app",
  wildcard = true,
}: {
  value: string
  onChange: (hostname: string) => void
  invalid?: boolean
  autoFocus?: boolean
  placeholder?: string
  /** Whether `*` (every subdomain) is offered. */
  wildcard?: boolean
}) {
  const domains = useVerifiedDomains()
  const current = domains.find((domain) => isUnder(value, domain))
  // The picked domain; until one is picked, the first verified one (the list may load after mount).
  const [base, setBase] = useState<string>()
  const domain = current ?? (base && domains.includes(base) ? base : domains[0])

  if (!domain) {
    return (
      <>
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
        <p className="text-xs text-muted-foreground">
          <Link href="/domains" className="underline underline-offset-4 hover:text-foreground">
            Add your domains
          </Link>{" "}
          to pick them here.
        </p>
      </>
    )
  }

  const sub = value === domain ? "" : value.endsWith(`.${domain}`) ? value.slice(0, -domain.length - 1) : value
  const compose = (nextSub: string, nextDomain: string) => onChange(nextSub ? `${nextSub}.${nextDomain}` : nextDomain)

  return (
    <InputGroup>
      <InputGroupInput
        autoFocus={autoFocus}
        aria-label="Subdomain"
        placeholder={placeholder}
        value={sub}
        onChange={(event) =>
          compose(
            event.target.value
              .toLowerCase()
              .replace(wildcard ? /[^a-z0-9*.-]/g : /[^a-z0-9.-]/g, "")
              .replace(/^\.+/, ""),
            domain,
          )
        }
        aria-invalid={invalid}
        className="font-mono"
        autoComplete="off"
        spellCheck={false}
      />
      <InputGroupAddon align="inline-end" className="font-mono">
        <DropdownMenu>
          <DropdownMenuTrigger
            disabled={domains.length < 2}
            className="inline-flex items-center gap-0.5 rounded-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:pointer-events-none"
          >
            .{domain}
            {domains.length > 1 && <CaretDownIcon className="size-3 text-muted-foreground" />}
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="min-w-44 font-mono">
            <DropdownMenuRadioGroup
              value={domain}
              onValueChange={(next) => {
                setBase(next)
                compose(sub, next)
              }}
            >
              {domains.map((entry) => (
                <DropdownMenuRadioItem key={entry} value={entry}>
                  {entry}
                </DropdownMenuRadioItem>
              ))}
            </DropdownMenuRadioGroup>
          </DropdownMenuContent>
        </DropdownMenu>
      </InputGroupAddon>
    </InputGroup>
  )
}
