"use client"

import { CheckCircleIcon, CircleIcon } from "@phosphor-icons/react"
import type { Workspace } from "@/lib/api"
import { cn } from "@/lib/utils"

const roleLabel: Record<string, string> = { owner: "Owner", admin: "Admin", member: "Member" }

/**
 * Choose where a new machine goes: Personal or one of your teams. `brand` is for the blue approval
 * screens, `default` for dialogs.
 */
export function WorkspacePicker({
  workspaces,
  value,
  onChange,
  tone = "default",
  disabled,
}: {
  workspaces: Workspace[]
  value: string
  onChange: (id: string) => void
  tone?: "brand" | "default"
  disabled?: boolean
}) {
  const brand = tone === "brand"
  return (
    <div role="radiogroup" aria-label="Workspace" className="flex flex-col gap-1.5">
      {workspaces.map((workspace) => {
        const selected = workspace.id === value
        const Icon = selected ? CheckCircleIcon : CircleIcon
        return (
          <button
            key={workspace.id}
            type="button"
            role="radio"
            aria-checked={selected}
            disabled={disabled}
            onClick={() => onChange(workspace.id)}
            className={cn(
              "flex h-11 items-center gap-3 rounded-lg px-3 text-left text-sm outline-none focus-visible:ring-2 disabled:opacity-60",
              brand
                ? cn("focus-visible:ring-white/50", selected ? "bg-white/25" : "bg-white/10 hover:bg-white/15")
                : cn("focus-visible:ring-ring/30", selected ? "bg-accent" : "bg-muted hover:bg-accent"),
            )}
          >
            <Icon weight={selected ? "fill" : "regular"} className="size-4 shrink-0" />
            <span className="min-w-0 flex-1 truncate-x font-medium trim-cap">{workspace.name}</span>
            {workspace.kind === "team" && (
              <span className={cn("text-xs trim-cap", brand ? "text-white/75" : "text-muted-foreground")}>
                {workspace.approve_new_machines && workspace.role === "member"
                  ? "Needs an owner or admin to accept"
                  : (roleLabel[workspace.role] ?? workspace.role)}
              </span>
            )}
          </button>
        )
      })}
    </div>
  )
}
