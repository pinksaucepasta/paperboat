"use client"

import { KeyIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useApiQuery, useWorkspace } from "@/lib/api"
import {
  actionLabels,
  apiTokensQuery,
  createApiToken,
  machineActions,
  namePattern,
  notOnServer,
  PolicyError,
  policyQuery,
  revokeApiToken,
  type APIToken,
} from "@/lib/policy"
import { useNow } from "@/lib/use-now"
import { CopyButton } from "@/components/copy-button"
import { Time } from "@/components/time"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"

const expiries = [7, 30, 90] as const

/** Settings → API tokens: credentials for scripts and agents that act as you in this workspace. */
export function ApiTokens() {
  const workspace = useWorkspace()
  const tokens = useApiQuery(apiTokensQuery(workspace?.id ?? ""))
  const [creating, setCreating] = useState(false)
  const [revoking, setRevoking] = useState<APIToken | null>(null)
  const now = useNow(60_000)
  if (!workspace || tokens.isPending) return <Skeleton className="h-40 w-full" />
  if (tokens.isError) {
    const missing = tokens.error instanceof PolicyError && tokens.error.code === notOnServer
    return (
      <div className="flex flex-col items-center gap-3 surface rounded-lg px-6 py-12 text-center">
        <KeyIcon className="size-8 text-muted-foreground" />
        <p className="font-medium">
          {missing ? "API tokens aren't available on this server yet" : "Couldn't load API tokens"}
        </p>
        {!missing && <p className="max-w-sm text-sm text-muted-foreground">{tokens.error.message}</p>}
      </div>
    )
  }
  return (
    <div className="flex flex-col gap-6">
      <header className="flex items-center gap-4">
        <h1 className="flex-1 font-heading text-2xl font-semibold tracking-tight">API tokens</h1>
        <Button size="lg" onClick={() => setCreating(true)}>
          Create token
        </Button>
      </header>
      {tokens.data.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No tokens. A token lets a script or agent act as you in {workspace.name}.
        </p>
      ) : (
        <ul className="flex flex-col border-t border-divider">
          {tokens.data.map((token) => {
            const expired = Date.parse(token.expires_at) < now
            return (
              <li
                key={token.id}
                className="flex min-h-12 flex-wrap items-center gap-3 border-b border-divider py-2 text-sm"
                data-token={token.name}
              >
                <span className="min-w-0 flex-1">
                  <span className="flex items-center gap-2 font-medium">
                    {token.name}
                    {token.revoked_at ? (
                      <Badge variant="outline" className="font-normal text-muted-foreground">
                        Revoked
                      </Badge>
                    ) : expired ? (
                      <Badge variant="outline" className="font-normal text-muted-foreground">
                        Expired
                      </Badge>
                    ) : null}
                  </span>
                  <span className="block text-muted-foreground">
                    {token.created_by.replace(/^user:/, "")} ·{" "}
                    {token.scope?.length ? scopeText(token) : "Everything you can do"} ·{" "}
                    {token.last_used_at ? (
                      <>
                        used <Time iso={token.last_used_at} relative />
                      </>
                    ) : (
                      "never used"
                    )}{" "}
                    · {expired ? "expired" : "expires"} <Time iso={token.expires_at} relative />
                  </span>
                </span>
                {!token.revoked_at && !expired && (
                  <Button variant="outline" onClick={() => setRevoking(token)}>
                    Revoke
                  </Button>
                )}
              </li>
            )
          })}
        </ul>
      )}
      <Dialog open={creating} onOpenChange={setCreating}>
        <DialogContent className="gap-4 sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Create API token</DialogTitle>
          </DialogHeader>
          {creating && <CreateToken workspace={workspace.id} />}
        </DialogContent>
      </Dialog>
      {revoking && <RevokeToken workspace={workspace.id} token={revoking} onClose={() => setRevoking(null)} />}
    </div>
  )
}

function scopeText(token: APIToken) {
  return (token.scope ?? [])
    .map((entry) => `${entry.allow.map((a) => actionLabels[a] ?? a).join(", ")} on ${entry.dst.join(", ")}`)
    .join("; ")
}

function CreateToken({ workspace }: { workspace: string }) {
  const queryClient = useQueryClient()
  const policy = useApiQuery(policyQuery(workspace))
  const [name, setName] = useState("")
  const [days, setDays] = useState<number>(90)
  const [limited, setLimited] = useState(false)
  const [targets, setTargets] = useState<string[]>([])
  const [actions, setActions] = useState<string[]>([])
  const scopeReady = !limited || (targets.length > 0 && actions.length > 0)
  const valid = namePattern.test(name) && scopeReady
  const create = useMutation({
    mutationFn: () =>
      createApiToken(workspace, {
        name,
        expires_in_days: days,
        ...(limited ? { scope: [{ dst: targets, allow: actions }] } : {}),
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: apiTokensQuery(workspace).queryKey }),
  })

  if (create.isSuccess)
    return (
      <div className="flex flex-col gap-3">
        <p className="text-sm">Copy the token now. It is not shown again.</p>
        <div className="flex items-center gap-2 surface rounded-md px-3 py-2">
          <code className="min-w-0 flex-1 truncate font-mono text-xs" data-token-secret>
            {create.data.secret}
          </code>
          <CopyButton value={create.data.secret} />
        </div>
        <span className="text-xs text-muted-foreground">
          Send it as Authorization: Bearer on API requests.
        </span>
      </div>
    )

  const names = policy.data?.names
  const choices = [...(names?.tags ?? []), ...(names?.machines ?? []).map((m) => `machine:${m}`)]
  const toggle = (list: string[], value: string, on: boolean) =>
    on ? [...list, value] : list.filter((v) => v !== value)
  const problems = create.error instanceof PolicyError ? (create.error.details.errors ?? []) : []
  return (
    <form
      className="flex flex-col gap-4"
      onSubmit={(event) => {
        event.preventDefault()
        if (valid) create.mutate()
      }}
    >
      <Field className="gap-1.5">
        <FieldLabel htmlFor="token-name">Name</FieldLabel>
        <Input
          id="token-name"
          autoFocus
          placeholder="ci-runner"
          value={name}
          onChange={(event) => setName(event.target.value.toLowerCase())}
          autoComplete="off"
          spellCheck={false}
        />
        <span className="text-xs text-muted-foreground">
          Lowercase letters, numbers, dots, dashes and underscores. Up to 63.
        </span>
      </Field>
      <Field className="gap-1.5">
        <FieldLabel htmlFor="token-expiry">Expires after</FieldLabel>
        <select
          id="token-expiry"
          className="h-9 rounded-md border border-input bg-transparent px-2.5 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"
          value={days}
          onChange={(event) => setDays(Number(event.target.value))}
        >
          {expiries.map((d) => (
            <option key={d} value={d}>
              {d} days
            </option>
          ))}
        </select>
      </Field>
      <div className="flex flex-col gap-3 border-t border-divider pt-4">
        <label className="flex cursor-pointer items-center gap-2.5 text-sm">
          <Checkbox checked={limited} onCheckedChange={(on) => setLimited(Boolean(on))} />
          <span className="font-medium">Limit what it can do</span>
        </label>
        {limited && (
          <>
            <Field className="gap-1.5">
              <FieldLabel>On</FieldLabel>
              {policy.isError ? (
                <span className="text-xs text-destructive">
                  Loading tags and machines failed: {policy.error.message}
                </span>
              ) : choices.length === 0 ? (
                <span className="text-xs text-muted-foreground">No tags or machines in this workspace yet.</span>
              ) : (
                <div className="flex max-h-36 flex-wrap gap-x-4 gap-y-2 overflow-y-auto">
                  {choices.map((target) => (
                    <label key={target} className="flex cursor-pointer items-center gap-2 text-sm">
                      <Checkbox
                        checked={targets.includes(target)}
                        onCheckedChange={(on) => setTargets(toggle(targets, target, Boolean(on)))}
                      />
                      {target.replace(/^machine:/, "")}
                    </label>
                  ))}
                </div>
              )}
            </Field>
            <Field className="gap-1.5">
              <FieldLabel>Allow</FieldLabel>
              <div className="flex flex-wrap gap-x-4 gap-y-2">
                {machineActions.map((action) => (
                  <label key={action} className="flex cursor-pointer items-center gap-2 text-sm">
                    <Checkbox
                      checked={actions.includes(action)}
                      onCheckedChange={(on) => setActions(toggle(actions, action, Boolean(on)))}
                    />
                    {actionLabels[action]}
                  </label>
                ))}
              </div>
              <span className="text-xs text-muted-foreground">Never more than you can do yourself.</span>
            </Field>
          </>
        )}
      </div>
      {create.isError && <p className="text-sm text-destructive">{create.error.message}</p>}
      {problems.length > 0 && (
        <ul className="text-sm text-destructive">
          {problems.map((problem, index) => (
            <li key={index}>
              <span className="font-mono text-xs">{problem.path}</span> {problem.message}
            </li>
          ))}
        </ul>
      )}
      <DialogFooter>
        <Button type="submit" size="lg" disabled={!valid || create.isPending}>
          {create.isPending ? "Creating…" : "Create token"}
        </Button>
      </DialogFooter>
    </form>
  )
}

function RevokeToken({ workspace, token, onClose }: { workspace: string; token: APIToken; onClose: () => void }) {
  const queryClient = useQueryClient()
  const revoke = useMutation({
    mutationFn: () => revokeApiToken(workspace, token.id),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: apiTokensQuery(workspace).queryKey })
      onClose()
    },
  })
  return (
    <AlertDialog open onOpenChange={(open) => !open && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Revoke {token.name}?</AlertDialogTitle>
        </AlertDialogHeader>
        <p className="text-sm text-muted-foreground">Scripts and agents using it stop working at once.</p>
        {revoke.isError && <p className="text-sm text-destructive">{revoke.error.message}</p>}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={revoke.isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={revoke.isPending}
            onClick={(event) => {
              event.preventDefault()
              revoke.mutate()
            }}
          >
            {revoke.isPending ? "Revoking…" : "Revoke token"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
