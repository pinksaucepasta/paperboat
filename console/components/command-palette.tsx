"use client"

import { MagnifyingGlassIcon, UserCircleIcon } from "@phosphor-icons/react"
import { useRouter } from "next/navigation"
import { useEffect, useState } from "react"
import { useDebounced } from "@/hooks/use-debounced"
import { machineSearchQuery, useApiQuery, useMachinePage, useWorkspace, type Machine } from "@/lib/api"
import { navigationGroups } from "@/lib/sections"
import { StatusDot } from "@/components/machine-status"
import {
  Command,
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command"
import { Kbd } from "@/components/ui/kbd"

const pages = navigationGroups.flatMap((group) => group.items)

/** ⌘K / Ctrl+K: jump to any page or machine. Machine search runs on the server, so size does not matter. */
export function CommandPalette() {
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState("")
  const query = useDebounced(search.trim(), 200)
  const router = useRouter()
  const workspace = useWorkspace()
  const page = useMachinePage()
  const results = useApiQuery({
    ...machineSearchQuery(workspace?.id ?? "", query),
    enabled: open && Boolean(workspace) && query.length > 0,
    placeholderData: (previous) => previous,
  })

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key.toLowerCase() === "k" && (event.metaKey || event.ctrlKey)) {
        event.preventDefault()
        setOpen((current) => !current)
      }
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [])

  const go = (href: string) => {
    setOpen(false)
    setSearch("")
    router.push(href)
  }

  const needle = search.trim().toLowerCase()
  const matchingPages = pages.filter((section) => !needle || section.title.toLowerCase().includes(needle))
  const machines: Machine[] = query ? (results.data?.items ?? []) : (page.data?.items.filter((machine) => machine.state !== "revoked").slice(0, 5) ?? [])

  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className="hidden h-8 w-60 items-center gap-2 rounded-md bg-surface px-2.5 text-sm text-muted-foreground transition-colors hover:text-foreground md:flex"
      >
        <MagnifyingGlassIcon className="size-4" />
        <span className="flex-1 text-left">Search</span>
        <Kbd>⌘K</Kbd>
      </button>
      <CommandDialog
        open={open}
        onOpenChange={(next) => {
          setOpen(next)
          if (!next) setSearch("")
        }}
        title="Search"
        description="Jump to a page or machine"
        className="top-[12vh]"
      >
        <Command shouldFilter={false}>
          <CommandInput placeholder="Search pages and machines" value={search} onValueChange={setSearch} />
          <CommandList className="max-h-96">
            <CommandEmpty>{results.isFetching ? "Searching…" : "Nothing found"}</CommandEmpty>
            {machines.length > 0 && (
              <CommandGroup heading="Machines">
                {machines.map((machine) => (
                  <CommandItem
                    key={machine.id}
                    value={`machine:${machine.id}`}
                    onSelect={() => go(`/machines/${machine.id}`)}
                  >
                    <span className="flex min-w-0 flex-1 items-center gap-2">
                      <span className="truncate-x trim-cap">{machine.alias}</span>
                      <StatusDot online={machine.online} />
                    </span>
                    {machine.assigned_ip && (
                      <span className="font-mono text-xs text-muted-foreground">{machine.assigned_ip}</span>
                    )}
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            {matchingPages.length > 0 && (
              <CommandGroup heading="Pages">
                {matchingPages.map(({ href, title, icon: Icon }) => (
                  <CommandItem key={href} value={`page:${href}`} onSelect={() => go(href)}>
                    <Icon className="text-muted-foreground" />
                    {title}
                  </CommandItem>
                ))}
                {(!needle || "account".includes(needle)) && (
                  <CommandItem value="page:/account" onSelect={() => go("/account")}>
                    <UserCircleIcon className="text-muted-foreground" />
                    Account
                  </CommandItem>
                )}
              </CommandGroup>
            )}
          </CommandList>
        </Command>
      </CommandDialog>
    </>
  )
}
