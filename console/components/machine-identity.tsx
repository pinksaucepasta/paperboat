"use client"

import { CheckIcon, CircleNotchIcon, PencilSimpleIcon, XIcon } from "@phosphor-icons/react"
import { useRouter } from "next/navigation"
import { useState } from "react"
import type { Machine } from "@/lib/api"
import { useMachineHostnames } from "@/lib/machine-domains"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { useMachineEdit } from "@/components/machine-edit"
import { TagChips } from "@/components/machine-tags"
import { RemoveMachineDialog } from "@/components/machine-remove"

/**
 * The machine's name and tags. Hovering shows a pencil; editing happens in the title
 * itself (same type, a faint underline), with Enter to save and Escape to cancel.
 */
export function MachineTitle({ machine }: { machine: Machine }) {
  const [editing, setEditing] = useState(false)
  if (editing) return <TitleEditor machine={machine} onDone={() => setEditing(false)} />
  return (
    <div className="group/title flex min-w-0 flex-col gap-1">
      <div className="flex min-w-0 items-center gap-1">
        <h1 className="truncate font-heading text-2xl font-semibold tracking-tight">{machine.alias}</h1>
        {machine.can_manage && (
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label="Edit name"
            onClick={() => setEditing(true)}
            className="text-muted-foreground opacity-0 transition-opacity group-hover/title:opacity-100 focus-visible:opacity-100"
          >
            <PencilSimpleIcon className="size-4" />
          </Button>
        )}
      </div>
      <TagChips tags={machine.tags} />
    </div>
  )
}

const inPlace =
  "h-auto rounded-none border-0 border-b border-dashed border-border bg-transparent px-0 shadow-none focus-visible:border-ring focus-visible:ring-0 dark:bg-transparent"

function TitleEditor({
  machine,
  onDone,
}: {
  machine: Machine
  onDone: () => void
}) {
  const edit = useMachineEdit(machine, onDone)
  const [hostname] = useMachineHostnames(edit.finalName)
  const nameChanged = edit.finalName !== machine.alias
  return (
    <form
      className="flex min-w-0 flex-col gap-1"
      onSubmit={(event) => {
        event.preventDefault()
        edit.submit()
      }}
      onKeyDown={(event) => event.key === "Escape" && !edit.save.isPending && onDone()}
    >
      <div className="flex min-w-0 items-center gap-1">
        <Input
          autoFocus
          aria-label="Name"
          value={edit.name}
          onChange={(event) => edit.setName(event.target.value)}
          aria-invalid={Boolean(edit.problem)}
          size={Math.max(edit.name.length, 4)}
          className={`${inPlace} w-auto py-0 font-heading text-2xl font-semibold tracking-tight md:text-2xl`}
          autoComplete="off"
          spellCheck={false}
        />
        <Button
          type="submit"
          variant="ghost"
          size="icon-sm"
          aria-label="Save"
          disabled={Boolean(edit.problem) || edit.save.isPending}
        >
          {edit.save.isPending ? <CircleNotchIcon className="size-4 animate-spin" /> : <CheckIcon className="size-4" />}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          aria-label="Cancel"
          onClick={onDone}
          disabled={edit.save.isPending}
        >
          <XIcon className="size-4" />
        </Button>
      </div>
      {(edit.problem || edit.save.isError || nameChanged) && (
        <p className={edit.problem || edit.save.isError ? "text-xs text-destructive" : "text-xs text-muted-foreground"}>
          {edit.save.isError
            ? edit.save.error.message
            : (edit.problem ??
              (hostname && (
                <>
                  Reachable as <span className="font-mono">{hostname}</span>
                </>
              )))}
        </p>
      )}
    </form>
  )
}

/** The end of the Overview, set apart like GitHub's and Vercel's danger zones. */
export function DangerZone({ machine }: { machine: Machine }) {
  const router = useRouter()
  const [confirming, setConfirming] = useState(false)
  if (!machine.can_manage) return null

  return (
    <section className="flex flex-col gap-3">
      <h2 className="text-sm font-medium text-destructive">Danger zone</h2>
      {/* A faint red fill, no frame; the button is a light chip with red text. */}
      <div className="flex flex-wrap items-center gap-4 rounded-lg bg-destructive/5 p-4 dark:bg-destructive/10">
        <div className="flex min-w-0 flex-1 flex-col gap-0.5">
          <p className="text-sm font-medium">Remove machine</p>
          <p className="text-sm text-muted-foreground">It loses access right away and its open sessions end.</p>
        </div>
        <Button
          variant="secondary"
          size="lg"
          className="bg-background text-destructive hover:bg-background/70 dark:bg-white/10 dark:hover:bg-white/15"
          onClick={() => setConfirming(true)}
        >
          Remove
        </Button>
      </div>
      {confirming && (
        <RemoveMachineDialog
          machine={machine}
          onClose={() => setConfirming(false)}
          onRemoved={() => router.push("/")}
        />
      )}
    </section>
  )
}
