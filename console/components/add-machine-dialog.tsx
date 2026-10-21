"use client"

import { CheckCircleIcon, CircleNotchIcon, PlusIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useEffect, useRef, useState } from "react"
import { machineOverviewQuery, useApiQuery, useWorkspace, workspacesQuery, type Workspace } from "@/lib/api"
import {
  cancelEnrollment,
  describeMachine,
  enrollmentQuery,
  installCommand,
  installUrlFrom,
  nameProblem,
  normalizeName,
  startEnrollment,
} from "@/lib/enrollment"
import { CopyIcons, useCopy } from "@/components/copy-button"
import { ShareMachine } from "@/components/share-machine-dialog"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NewMachineAccess } from "@/components/team/new-machine-access"
import { WorkspacePicker } from "@/components/workspace-picker"

/** Install on a new computer in any workspace; in a team, also share one of your machines. */
export function AddMachineDialog() {
  const workspace = useWorkspace()
  const { data } = useApiQuery(machineOverviewQuery)
  const installUrl = installUrlFrom(data?.bootstrap_command)
  const [open, setOpen] = useState(false)
  if (!installUrl || !workspace) return null

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="lg" />}>
        <PlusIcon data-icon="inline-start" />
        Add machine
      </DialogTrigger>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add a machine</DialogTitle>
        </DialogHeader>
        {open && <AddMachineChoices installUrl={installUrl} workspace={workspace} onDone={() => setOpen(false)} />}
      </DialogContent>
    </Dialog>
  )
}

/** Where the machine goes, then its command. Changing the workspace starts a new enrollment. */
function AddMachineChoices({
  installUrl,
  workspace,
  onDone,
}: {
  installUrl: string
  workspace: Workspace
  onDone: () => void
}) {
  const { data: workspaces } = useApiQuery(workspacesQuery)
  const [target, setTarget] = useState(workspace.id)
  const [plain, setPlain] = useState(false)
  const [sharing, setSharing] = useState(false)
  const destination =
    workspaces?.find((entry) => entry.id === target) ?? (target === workspace.id ? workspace : undefined)
  if (sharing && workspace.kind === "team") return <ShareMachine team={workspace} onDone={onDone} />
  return (
    <div className="flex flex-col gap-4">
      {!plain && workspaces && workspaces.length > 1 && (
        <Field className="gap-1.5">
          <FieldLabel>Workspace</FieldLabel>
          <WorkspacePicker workspaces={workspaces} value={target} onChange={setTarget} />
        </Field>
      )}
      <AddMachine
        key={target}
        installUrl={installUrl}
        workspace={target}
        plain={plain}
        onTogglePlain={() => setPlain(!plain)}
        onDone={onDone}
      />
      {destination?.kind === "team" && (
        <NewMachineAccess team={destination} current={destination.id === workspace.id} />
      )}
      {workspace.kind === "team" && (
        <button
          type="button"
          onClick={() => setSharing(true)}
          className="self-start text-xs text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
        >
          Share a machine you already have
        </button>
      )}
    </div>
  )
}

function AddMachine({
  installUrl,
  workspace,
  plain,
  onTogglePlain,
  onDone,
}: {
  installUrl: string
  workspace: string
  plain: boolean
  onTogglePlain: () => void
  onDone: () => void
}) {
  const queryClient = useQueryClient()
  // Launch is macOS and Linux only; Windows comes in the first update (PLAN.md 1.1).
  const shell = "posix"
  const [name, setName] = useState("")
  const [description, setDescription] = useState("")

  // The command is the point of this dialog, so a pairing token is requested as soon as it opens.
  // A token that arrives after the dialog or workspace choice is gone is cancelled on arrival.
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])
  const start = useMutation({
    mutationFn: async (input: Parameters<typeof startEnrollment>[0]) => {
      const created = await startEnrollment(input)
      if (!alive.current) void cancelEnrollment(created.id).catch(() => {})
      return created
    },
  })
  const enrollment = start.data
  const requested = useRef(false)
  useEffect(() => {
    // Exactly one request, even when effects run twice in development.
    if (requested.current) return
    requested.current = true
    start.mutate({ shell, workspace })
  }, [start, workspace])

  const live = useApiQuery({ ...enrollmentQuery(enrollment?.id ?? ""), enabled: Boolean(enrollment) })
  const state = live.data?.state ?? enrollment?.state
  const machineId = live.data?.machine_id

  // Closing the dialog before the machine pairs revokes the unused token.
  const pending = useRef<string | undefined>(undefined)
  useEffect(() => {
    pending.current = enrollment && state !== "ready" ? enrollment.id : undefined
  }, [enrollment, state])
  useEffect(
    () => () => {
      if (pending.current) void cancelEnrollment(pending.current).catch(() => {})
    },
    [],
  )

  const describe = useMutation({ mutationFn: () => describeMachine(machineId!, description.trim()) })
  useEffect(() => {
    if (state !== "ready" || !machineId) return
    void queryClient.invalidateQueries({ queryKey: ["machines"] })
    if (description.trim() && describe.isIdle) describe.mutate()
  }, [state, machineId, description, describe, queryClient])

  const finalName = name.replace(/-+$/, "")
  const problem = nameProblem(name)
  const command = plain
    ? installCommand(installUrl, shell)
    : enrollment && installCommand(installUrl, shell, enrollment.bootstrap_token, finalName || undefined)

  if (state === "ready" && machineId) {
    return (
      <div className="flex flex-col items-center gap-3 py-6 text-center">
        <CheckCircleIcon weight="fill" className="size-10 text-success" />
        <p className="font-medium">Machine connected</p>
        {describe.isError && <p className="text-sm text-destructive">The description could not be saved.</p>}
        <Button size="lg" className="mt-1" render={<Link href={`/machines/${machineId}`} onClick={onDone} />}>
          Open machine
        </Button>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-2">
        {command ? (
          <CommandBlock command={command} />
        ) : (
          <div className="h-[4.25rem] animate-pulse rounded-lg bg-muted" />
        )}
        <div className="flex min-h-5 items-center justify-between gap-3 text-xs text-muted-foreground">
          {plain ? (
            <span>Installs, then opens your browser to sign in, choose the workspace and approve.</span>
          ) : start.isError ? (
            <span className="text-destructive">{start.error.message}</span>
          ) : (
            <EnrollmentStatus state={state} onRetry={() => start.mutate({ shell, workspace })} />
          )}
          <button
            type="button"
            onClick={onTogglePlain}
            className="shrink-0 underline-offset-4 hover:text-foreground hover:underline"
          >
            {plain ? "Use a pairing command" : "Approve in browser instead"}
          </button>
        </div>
      </div>

      {!plain && (
        <div className="flex flex-col gap-3 border-t border-divider pt-4">
          <Field data-invalid={Boolean(problem)} className="gap-1.5">
            <OptionalLabel htmlFor="machine-name">Name</OptionalLabel>
            <Input
              id="machine-name"
              placeholder="Uses the computer's name"
              value={name}
              onChange={(event) => setName(normalizeName(event.target.value))}
              aria-invalid={Boolean(problem)}
              autoComplete="off"
              spellCheck={false}
            />
            {problem ? (
              <FieldError>{problem}</FieldError>
            ) : (
              <p className="text-xs text-muted-foreground">
                Keep it short, like <span className="font-mono">homelab</span> or{" "}
                <span className="font-mono">studio</span>. Lowercase letters, numbers and dashes.
              </p>
            )}
          </Field>
          <Field className="gap-1.5">
            <OptionalLabel htmlFor="machine-description">Description</OptionalLabel>
            <Input
              id="machine-description"
              placeholder="What this machine is for"
              maxLength={160}
              value={description}
              onChange={(event) => setDescription(event.target.value.replace(/\p{Cc}/gu, ""))}
              autoComplete="off"
            />
          </Field>
        </div>
      )}
    </div>
  )
}

function OptionalLabel({ htmlFor, children }: { htmlFor: string; children: React.ReactNode }) {
  return (
    <FieldLabel htmlFor={htmlFor} className="flex items-baseline justify-between">
      {children}
      <span className="text-xs font-normal text-muted-foreground">Optional</span>
    </FieldLabel>
  )
}

function EnrollmentStatus({ state, onRetry }: { state: string | undefined; onRetry: () => void }) {
  if (state === "expired" || state === "cancelled" || state === "denied" || state === "failed_retryable") {
    return (
      <span className="flex items-center gap-2">
        {state === "expired" ? "This command expired." : "Pairing did not finish."}
        <button type="button" onClick={onRetry} className="text-foreground underline-offset-4 hover:underline">
          New command
        </button>
      </span>
    )
  }
  const label =
    state === "installing" ? "Installing…" : state === "connecting" ? "Connecting…" : "Waiting for the machine…"
  return (
    <span className="flex items-center gap-1.5">
      <CircleNotchIcon className="size-3.5 animate-spin" />
      {label}
    </span>
  )
}

/** The command is the click target: clicking anywhere on it copies it. */
export function CommandBlock({ command }: { command: string }) {
  const { copied, copy } = useCopy(command)
  return (
    <button
      type="button"
      onClick={copy}
      aria-label={copied ? "Copied" : "Copy command"}
      className="group/copy flex w-full items-start gap-3 rounded-lg bg-muted py-3 pr-3 pl-3.5 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/30"
    >
      <code className="min-w-0 flex-1 font-mono text-xs leading-relaxed break-all">{command}</code>
      <CopyIcons
        copied={copied}
        className={copied ? undefined : "text-muted-foreground group-hover/copy:text-foreground"}
      />
    </button>
  )
}
