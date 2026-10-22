"use client"

import { useRouter } from "next/navigation"
import { useState } from "react"
import { needsSso, useApiQuery, useWorkspace, workspacesQuery } from "@/lib/api"
import { setWorkspace } from "@/lib/workspace"
import { cn } from "@/lib/utils"
import { CrumbPickerTrigger, pickerOption } from "@/components/crumb-picker"
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command"
import { Popover, PopoverContent } from "@/components/ui/popover"
import { Skeleton } from "@/components/ui/skeleton"
import { roleLabels } from "@/lib/teams"

/**
 * First breadcrumb on every page: which workspace everything to its right belongs to. Built
 * like the machine picker (same trigger, search and rows) so the two crumbs behave alike.
 */
export function WorkspaceSwitcher() {
  const { data: workspaces } = useApiQuery(workspacesQuery)
  const current = useWorkspace()
  const router = useRouter()
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState("")

  if (!current) return <Skeleton className="h-5 w-24" />

  const query = search.trim().toLowerCase()
  const shown = workspaces?.filter((workspace) => workspace.name.toLowerCase().includes(query)) ?? []

  const choose = (id: string) => {
    setOpen(false)
    setSearch("")
    if (id === current.id) return
    setWorkspace(id)
    // A machine page belongs to the workspace it was opened from.
    if (window.location.pathname.startsWith("/machines/")) router.push("/")
  }

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) setSearch("")
      }}
    >
      <CrumbPickerTrigger>
        <WorkspaceAvatar name={current.name} />
        <span className="hidden max-w-40 truncate-x trim-cap sm:inline">{current.name}</span>
      </CrumbPickerTrigger>
      <PopoverContent align="start" className="w-64 gap-0 p-0">
        <Command shouldFilter={false} defaultValue={current.id}>
          <CommandInput placeholder="Search workspaces" value={search} onValueChange={setSearch} />
          <CommandList>
            <CommandEmpty>No workspaces found</CommandEmpty>
            {shown.length > 0 && (
              <CommandGroup>
                {shown.map((workspace) => (
                  <CommandItem
                    key={workspace.id}
                    value={workspace.id}
                    className={cn(pickerOption, workspace.id === current.id && "font-semibold")}
                    onSelect={() => choose(workspace.id)}
                  >
                    <WorkspaceAvatar name={workspace.name} />
                    <span className="min-w-0 flex-1 truncate-x trim-cap">{workspace.name}</span>
                    <span className="text-xs font-normal text-muted-foreground">
                      {needsSso(workspace) ? "Needs SSO sign-in" : roleLabels[workspace.role]}
                    </span>
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  )
}


function WorkspaceAvatar({ name }: { name: string }) {
  return (
    <span
      aria-hidden
      className="flex size-5 shrink-0 items-center justify-center rounded-sm bg-primary/12 text-[10px] font-semibold text-primary"
    >
      {name.slice(0, 1).toUpperCase()}
    </span>
  )
}
