"use client"

import { CaretRightIcon, KeyIcon, PlusIcon } from "@phosphor-icons/react"
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query"
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
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ListSkeleton } from "@/components/skeletons"

const expiries = [7, 30, 90] as const

/** Settings → API tokens: credentials for scripts and agents that act as you in this workspace. */
export function ApiTokens() {
  const workspace = useWorkspace()
  const tokens = useInfiniteQuery(apiTokensQuery(workspace?.id ?? ""))
  const items = tokens.data?.pages.flatMap((page) => page.api_tokens) ?? []
  const total = tokens.data?.pages[0]?.total ?? 0
  const [creating, setCreating] = useState(false)
  const [revoking, setRevoking] = useState<APIToken | null>(null)
  const [showEnded, setShowEnded] = useState(false)
  const now = useNow(60_000)
  const create = (
    <Button size="lg" onClick={() => setCreating(true)}>
      <PlusIcon data-icon="inline-start" />
      Create token
    </Button>
  )
  if (!workspace || tokens.isPending) {
    return (
      <section className="flex flex-col gap-3">
        <div className="flex items-center">
          <h2 className="text-sm font-medium">Active</h2>
          <div className="ml-auto">{create}</div>
        </div>
        <ListSkeleton rows={3} detail="w-[36rem]" action />
      </section>
    )
  }
  if (tokens.isError) {
    const missing = tokens.error instanceof PolicyError && tokens.error.code === notOnServer
    return (
      <Empty className="py-16">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <KeyIcon />
          </EmptyMedia>
          <EmptyTitle>{missing ? "API tokens aren’t available on this server yet" : "Couldn’t load API tokens"}</EmptyTitle>
          {!missing && <EmptyDescription>{tokens.error.message}</EmptyDescription>}
        </EmptyHeader>
      </Empty>
    )
  }
  // Active tokens first; revoked and expired ones are history, folded away below them. The server has
  // no state filter, so this splits the pages loaded so far.
  const active = items.filter((token) => !token.revoked_at && Date.parse(token.expires_at) >= now)
  const ended = items.filter((token) => !active.includes(token))
  return (
    <div className="flex flex-col gap-6">
      {active.length === 0 && !tokens.hasNextPage ? (
        <Empty className="py-12">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <KeyIcon />
            </EmptyMedia>
            <EmptyTitle>No active tokens</EmptyTitle>
            <EmptyDescription>
              A token lets a script or agent act as you in {workspace.name}, limited to what you allow.
            </EmptyDescription>
          </EmptyHeader>
          <EmptyContent>{create}</EmptyContent>
        </Empty>
      ) : (
        <section className="flex flex-col gap-3">
          <div className="flex items-center">
            <h2 className="text-sm font-medium">Active</h2>
            <div className="ml-auto">{create}</div>
          </div>
          <TokenList tokens={active} now={now} onRevoke={setRevoking} />
        </section>
      )}
      {ended.length > 0 && (
        <section className="flex flex-col gap-3">
          <Button variant="ghost" className="self-start px-0 text-muted-foreground" onClick={() => setShowEnded(!showEnded)}>
            <CaretRightIcon data-icon="inline-start" className={showEnded ? "rotate-90" : undefined} />
            Revoked and expired · {ended.length}
          </Button>
          {showEnded && <TokenList tokens={ended} now={now} onRevoke={setRevoking} />}
        </section>
      )}
      {tokens.hasNextPage && (
        <div className="flex items-center gap-3 text-sm text-muted-foreground">
          <span>
            {items.length} of {total} tokens loaded
          </span>
          <Button variant="secondary" onClick={() => tokens.fetchNextPage()} disabled={tokens.isFetchingNextPage}>
            {tokens.isFetchingNextPage ? "Loading…" : "Load more"}
          </Button>
          {tokens.isFetchNextPageError && <span className="text-destructive">Couldn’t load more. Try again.</span>}
        </div>
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
        <span className="text-xs text-muted-foreground">Send it as Authorization: Bearer on API requests.</span>
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

function TokenList({ tokens, now, onRevoke }: { tokens: APIToken[]; now: number; onRevoke: (token: APIToken) => void }) {
  return (
    <ul className="flex flex-col border-t border-divider">
      {tokens.map((token) => {
        const expired = Date.parse(token.expires_at) < now
        return (
          <li
            key={token.id}
            className="flex min-h-14 flex-wrap items-center gap-3 border-b border-divider py-2 text-sm"
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
              <span className="block truncate text-muted-foreground">
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
              <Button variant="outline" onClick={() => onRevoke(token)}>
                Revoke
              </Button>
            )}
          </li>
        )
      })}
    </ul>
  )
}
