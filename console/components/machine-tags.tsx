"use client"

import { PlusIcon, TagIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useState } from "react"
import { meQuery, useApiQuery, useWorkspace, type Machine } from "@/lib/api"
import { PolicyError, setMachineTags, tagNamePattern, tagsLive, tagsQuery, type AccessChange } from "@/lib/policy"
import { ChangeList } from "@/components/access/changes-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"

/** "tag:prod" → "prod": the prefix belongs to the policy file, not to what people read. */
export const tagLabel = (tag: string) => tag.replace(/^tag:/, "")

/** A machine's tags as small chips; each filters the machines list by that tag. */
export function TagChips({ tags, className }: { tags?: string[]; className?: string }) {
  if (!tagsLive || !tags?.length) return null
  return (
    <span className={`inline-flex flex-wrap gap-1 ${className ?? ""}`}>
      {tags.map((tag) => (
        <Badge key={tag} variant="outline" className="font-normal text-muted-foreground" render={<Link href={`/?q=${encodeURIComponent(`tag:${tagLabel(tag)}`)}`} />}>
          {tagLabel(tag)}
        </Badge>
      ))}
    </span>
  )
}

/** Whether the signed-in person may change this machine's tags (owners/admins any; members their own). */
function useCanEdit(machine: Machine) {
  const workspace = useWorkspace()
  const tags = useApiQuery(tagsQuery(workspace?.id ?? ""))
  const me = useApiQuery(meQuery).data
  const rights = tags.data?.can_edit
  return rights === "all" || (rights === "own_machines" && Boolean(me) && machine.owner_account === me?.id)
}

/** The machine page's tags with "Edit tags". */
export function MachineTags({ machine }: { machine: Machine }) {
  const [editing, setEditing] = useState(false)
  const canEdit = useCanEdit(machine)
  if (!tagsLive) return null
  return (
    <span className="inline-flex flex-wrap items-center gap-2" data-machine-tags>
      <TagIcon className="size-4 text-muted-foreground" aria-hidden />
      {machine.tags?.length ? <TagChips tags={machine.tags} /> : <span className="text-muted-foreground">No tags</span>}
      {canEdit && (
        <Button variant="ghost" size="sm" onClick={() => setEditing(true)}>
          Edit tags
        </Button>
      )}
      {editing && <EditTags machine={machine} onClose={() => setEditing(false)} />}
    </span>
  )
}

function EditTags({ machine, onClose }: { machine: Machine; onClose: () => void }) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()!
  const all = useApiQuery(tagsQuery(workspace.id))
  const [chosen, setChosen] = useState<string[]>(machine.tags ?? [])
  const [typed, setTyped] = useState("")
  const [review, setReview] = useState<{ changes: AccessChange[]; confirm: string } | null>(null)
  const names = [...new Set([...(all.data?.tags.map((tag) => tag.name) ?? []), ...chosen])].sort()
  const candidate = `tag:${typed.trim().toLowerCase().replace(/^tag:/, "")}`
  const typedValid = typed.trim() !== "" && tagNamePattern.test(candidate)
  const save = useMutation({
    mutationFn: (confirm?: string) => setMachineTags(workspace.id, machine.id, chosen, all.data!.policy_version, confirm),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["policy", workspace.id] })
      await queryClient.invalidateQueries({ queryKey: ["machines"] })
      onClose()
    },
    onError: (error) => {
      // Tagging can change who may use the machine: the server lists it first, to confirm.
      if (error instanceof PolicyError && error.status === 412 && error.details.confirm)
        setReview({ changes: error.details.access_changes ?? [], confirm: error.details.confirm })
    },
  })
  const failure = save.error
  const conflict = failure instanceof PolicyError && failure.status === 409
  const toggle = (name: string, on: boolean) => setChosen(on ? [...chosen, name] : chosen.filter((tag) => tag !== name))

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Tags of {machine.alias}</DialogTitle>
        </DialogHeader>
        {review ? (
          <div className="flex flex-col gap-2 text-sm">
            <p>This changes who may use machines:</p>
            <ChangeList changes={review.changes} />
          </div>
        ) : (
          <>
            <div className="flex max-h-56 flex-col gap-1 overflow-y-auto">
              {names.length === 0 && <p className="text-sm text-muted-foreground">No tags yet. Create one below.</p>}
              {names.map((name) => (
                <label key={name} className="flex min-h-8 items-center gap-2 text-sm">
                  <Checkbox checked={chosen.includes(name)} onCheckedChange={(on) => toggle(name, Boolean(on))} />
                  {tagLabel(name)}
                </label>
              ))}
            </div>
            <form
              className="flex gap-2"
              onSubmit={(event) => {
                event.preventDefault()
                if (!typedValid) return
                if (!chosen.includes(candidate)) setChosen([...chosen, candidate])
                setTyped("")
              }}
            >
              <Input
                aria-label="New tag"
                placeholder="New tag, e.g. prod"
                value={typed}
                onChange={(event) => setTyped(event.target.value)}
                spellCheck={false}
                autoComplete="off"
              />
              <Button type="submit" variant="outline" disabled={!typedValid}>
                <PlusIcon data-icon="inline-start" />
                Add
              </Button>
            </form>
            {typed.trim() && !typedValid && (
              <p className="text-xs text-muted-foreground">Lowercase letters, digits, dots, dashes and underscores; start with a letter or digit.</p>
            )}
          </>
        )}
        {save.isError && !(failure instanceof PolicyError && failure.status === 412) && (
          <p className="text-sm text-destructive">
            {conflict ? "Someone changed the policy meanwhile. Close and open this again to see the current tags." : save.error.message}
          </p>
        )}
        <DialogFooter>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={save.isPending || !all.data || conflict}
            onClick={() => save.mutate(review?.confirm)}
          >
            {save.isPending ? "Saving…" : review ? "Save and change access" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
