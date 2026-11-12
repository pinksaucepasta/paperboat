"use client"

import { CaretDownIcon, CheckCircleIcon, CircleNotchIcon, PlusIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useEffect, useRef, useState } from "react"
import { ApiError, machineOverviewQuery, useApiQuery, useWorkspace, workspacesQuery, type Workspace } from "@/lib/api"
import {
  cancelEnrollment,
  enrollmentQuery,
  installCommand,
  installUrlFrom,
  nameProblem,
  normalizeName,
  startEnrollment,
  type Shell,
} from "@/lib/enrollment"
import { CopyIcons, useCopy } from "@/components/copy-button"
import { cn } from "@/lib/utils"
import { ShareMachine } from "@/components/share-machine-dialog"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { isLimitReached, LimitReached } from "@/components/limit-reached"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { NewMachineAccess } from "@/components/team/new-machine-access"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { roleLabels } from "@/lib/teams"

/** Install on a new computer in any workspace; in a team, also share one of your machines. */
export function AddMachineDialog() {
  const workspace = useWorkspace()
  const { data } = useApiQuery(machineOverviewQuery)
  const installUrl = installUrlFrom(data?.bootstrap_command)
  const [open, setOpen] = useState(false)
  const trigger = (
    <>
      <PlusIcon data-icon="inline-start" />
      Add machine
    </>
  )
  // Loaded without an install command: this workspace can't add machines here.
  if (data && !installUrl) return null

  // While the install command loads the button looks and works the same; the dialog waits for it.
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="lg" />}>{trigger}</DialogTrigger>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add a machine</DialogTitle>
        </DialogHeader>
        {open &&
          (installUrl && workspace ? (
            <AddMachineChoices installUrl={installUrl} workspace={workspace} onDone={() => setOpen(false)} />
          ) : (
            <Skeleton className="h-24 w-full" />
          ))}
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
          <WorkspaceSelect workspaces={workspaces} value={target} onChange={setTarget} />
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

/** One line for the destination: the current workspace, with the others a click away. */
function WorkspaceSelect({
  workspaces,
  value,
  onChange,
}: {
  workspaces: Workspace[]
  value: string
  onChange: (id: string) => void
}) {
  const note = (workspace: Workspace) =>
    workspace.kind !== "team"
      ? undefined
      : workspace.approve_new_machines && workspace.role === "member"
        ? "Needs an owner or admin to accept"
        : (roleLabels[workspace.role] ?? workspace.role)
  const selected = workspaces.find((workspace) => workspace.id === value)
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        aria-label="Workspace"
        className="flex h-7 w-full items-center gap-2 rounded-md border border-input bg-input/20 px-2 text-left text-xs/relaxed outline-none hover:bg-accent focus-visible:border-ring focus-visible:ring-2 dark:bg-input/30 focus-visible:ring-ring/30"
      >
        <span className="min-w-0 flex-1 truncate">{selected?.name}</span>
        {selected && note(selected) && (
          <span className="shrink-0 text-xs text-muted-foreground">{note(selected)}</span>
        )}
        <CaretDownIcon className="size-3.5 shrink-0 text-muted-foreground" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="max-h-72">
        <DropdownMenuRadioGroup value={value} onValueChange={(next) => onChange(String(next))}>
          {workspaces.map((workspace) => (
            <DropdownMenuRadioItem key={workspace.id} value={workspace.id}>
              <span className="min-w-0 flex-1 truncate">{workspace.name}</span>
              {note(workspace) && <span className="shrink-0 text-xs text-muted-foreground">{note(workspace)}</span>}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
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
  // The token is the same for every computer; the toggle only picks which installer path the command uses.
  const [shell, setShell] = useState<Shell>("posix")
  const [name, setName] = useState("")

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
    // One request, even when effects run twice in development.
    if (requested.current) return
    requested.current = true
    start.mutate({ workspace })
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

  useEffect(() => {
    if (state === "ready" && machineId) void queryClient.invalidateQueries({ queryKey: ["machines"] })
  }, [state, machineId, queryClient])

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
        <Button size="lg" className="mt-1" render={<Link href={`/machines/${machineId}`} onClick={onDone} />}>
          Open machine
        </Button>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-2">
        <ToggleGroup
          value={[shell]}
          onValueChange={(next) => next[0] && setShell(next[0] as Shell)}
          variant="outline"
          spacing={0}
          aria-label="Operating system of the new computer"
        >
          <ToggleGroupItem value="posix">macOS and Linux</ToggleGroupItem>
          <ToggleGroupItem value="powershell">Windows</ToggleGroupItem>
        </ToggleGroup>
        {command ? (
          <CommandBlock command={command} />
        ) : (
          <div className="h-[4.25rem] animate-pulse rounded-lg bg-muted" />
        )}
        <div className="flex min-h-5 items-center justify-between gap-3 text-xs text-muted-foreground">
          {plain ? (
            <span>Installs, then opens your browser to sign in, choose the workspace and approve.</span>
          ) : isLimitReached(start.error) ? (
            <LimitReached error={start.error} removeMachines />
          ) : start.isError ? (
            <span className="text-destructive">{start.error.message}</span>
          ) : (
            <EnrollmentStatus
              state={state}
              failure={live.data?.failure_code}
              onRetry={() => start.mutate({ workspace })}
            />
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
              <p className="text-xs text-muted-foreground">Lowercase letters, numbers and dashes.</p>
            )}
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

/** Pairing refused because the workspace reached its plan's machine limit (no billing_url here). */
const machineLimit = new ApiError(
  409,
  "machine_limit_reached",
  "This workspace has reached its plan's machine limit. Remove machines you no longer use, or upgrade your plan.",
)

function EnrollmentStatus({
  state,
  failure,
  onRetry,
}: {
  state: string | undefined
  failure?: string
  onRetry: () => void
}) {
  if (state === "failed_retryable" && failure === "machine_limit_reached")
    return <LimitReached error={machineLimit} removeMachines />
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

/**
 * The command is the click target: clicking anywhere on it copies it. A command never wraps: it
 * stays one line (one per command when there are several) and scrolls sideways under the copy
 * icon, which fades the text out beneath it.
 */
export function CommandBlock({
  command,
  label = "Copy command",
  wrap,
}: {
  command: string
  label?: string
  /** Wrap a long value (a code shown once) instead of scrolling it sideways. */
  wrap?: boolean
}) {
  const { copied, copy } = useCopy(command)
  // The text stays readable (screen readers read the command); only the copy icon is the labelled
  // button. A click anywhere on the block copies too, for the mouse.
  return (
    <div
      onClick={copy}
      className="group/copy relative flex w-full cursor-pointer rounded-lg bg-muted text-left hover:bg-accent has-focus-visible:ring-2 has-focus-visible:ring-ring/30"
    >
      <code
        className={cn(
          "no-scrollbar min-w-0 flex-1 py-3 pr-12 pl-3.5 font-mono text-xs leading-relaxed",
          wrap ? "break-all whitespace-normal" : "overflow-x-auto whitespace-pre"
        )}
      >
        {command}
      </code>
      <span className="pointer-events-none absolute inset-y-0 right-0 flex items-start rounded-r-lg bg-linear-to-l from-muted from-60% to-transparent py-3 pr-3 pl-8 group-hover/copy:from-accent">
        <button
          type="button"
          aria-label={copied ? "Copied" : label}
          onClick={(event) => {
            event.stopPropagation()
            copy()
          }}
          className="pointer-events-auto rounded-sm outline-none"
        >
          <CopyIcons
            copied={copied}
            className={copied ? undefined : "text-muted-foreground group-hover/copy:text-foreground"}
          />
        </button>
      </span>
    </div>
  )
}
