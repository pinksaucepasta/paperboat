"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { ApiError, useApiQuery } from "@/lib/api"
import {
  routesQuery,
  saveViewerPolicy,
  viewerPolicyQuery,
  type Tunnel,
  type ViewerPolicy,
  type ViewerTarget,
} from "@/lib/tunnels"
import { addViewers, isLimitedPublicRoute, savedViewers, viewerListProblem, viewerMutation } from "@/lib/viewer-access"
import { ErrorMessage } from "@/components/error-reference"
import { LoadError } from "@/components/load-error"
import { Section } from "@/components/section"
import { ChipField } from "@/components/tunnels/ip-rules"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { FieldError } from "@/components/ui/field"
import { Skeleton } from "@/components/ui/skeleton"
import { useShellState } from "@/lib/use-shell-state"

/** One list per HTTP route of a private or public tunnel. */
export function TunnelViewers({ tunnel }: { tunnel: Tunnel }) {
  const routes = useApiQuery(routesQuery(tunnel.id))
  if (tunnel.access_mode !== "private" && tunnel.access_mode !== "public") return null
  if (routes.isError)
    return (
      <Section title="Allowed viewers">
        <LoadError what="allowed viewers" error={routes.error} retry={() => routes.refetch()} />
      </Section>
    )
  const web = (routes.data ?? []).filter((route) => route.protocol === "http" && route.desired_state === "active")
  return web.map((route) => (
    <ViewerAccess
      key={route.id}
      target={{ kind: "tunnel", id: tunnel.id, routeId: route.id }}
      title={web.length > 1 ? `Allowed viewers: ${route.name}` : "Allowed viewers"}
      teamOwned={Boolean(tunnel.owner_team_id)}
      publicResource={tunnel.access_mode === "public"}
    />
  ))
}

/**
 * Who can open a private preview or an HTTP tunnel route besides its owner: email addresses and *@domain. A public
 * tunnel route stays open to anyone until a list is saved, and can be opened to anyone again.
 */
export function ViewerAccess({
  target,
  title = "Allowed viewers",
  teamOwned,
  publicResource = false,
}: {
  target: ViewerTarget
  title?: string
  teamOwned: boolean
  publicResource?: boolean
}) {
  const query = useApiQuery(viewerPolicyQuery(target))
  // A preview that takes no viewer policy (or just ended) answers 404: there is nothing to show.
  if (query.error instanceof ApiError && query.error.status === 404) return null
  if (query.isError)
    return (
      <Section title={title}>
        <LoadError what="allowed viewers" error={query.error} retry={() => query.refetch()} />
      </Section>
    )
  if (!query.data)
    return (
      <Section title={title}>
        <div className="pt-3">
          <Skeleton className="h-8 w-full" />
        </div>
      </Section>
    )
  // A saved policy remounts the form, so its draft starts from what the server holds.
  return (
    <ViewerForm
      key={query.data.generation}
      target={target}
      title={title}
      teamOwned={teamOwned}
      publicResource={publicResource}
      policy={query.data}
    />
  )
}

function ViewerForm({
  target,
  title,
  teamOwned,
  publicResource,
  policy,
}: {
  target: ViewerTarget
  title: string
  teamOwned: boolean
  publicResource: boolean
  policy: ViewerPolicy
}) {
  const queryClient = useQueryClient()
  const key = `tunnels/viewer-access.tsx:${target.kind}:${target.id}:${target.routeId}`
  const saved = savedViewers(policy)
  const limited = publicResource && isLimitedPublicRoute(policy)
  // An untouched empty list on an open route is not a change, so Save never limits it to the owner by itself.
  const open = publicResource && !limited
  const [draft, setDraft] = useShellState<string[]>(`${key}:draft`, saved)
  const [confirmOpen, setConfirmOpen] = useShellState(`${key}:open`, false)
  // A team audience (set outside the console) has no list; saving replaces it.
  const teamAudience = policy.configured && policy.audience === "team"
  const dirty = teamAudience || JSON.stringify(draft) !== JSON.stringify(saved)
  const problem = viewerListProblem(draft)
  const refresh = () => queryClient.invalidateQueries({ queryKey: viewerPolicyQuery(target).queryKey })
  const save = useMutation({
    mutationFn: (mutation: Parameters<typeof saveViewerPolicy>[1]) => saveViewerPolicy(target, mutation),
    onSuccess: async () => {
      await refresh()
      toast.success("Allowed viewers saved")
    },
    // Someone else saved first: show their list.
    onError: (error) => {
      if (error instanceof ApiError && error.code === "viewer_policy_conflict") void refresh()
    },
  })
  return (
    <Section
      title={title}
      action={
        <Button
          size="lg"
          disabled={!dirty || Boolean(problem) || save.isPending}
          onClick={() => save.mutate({ ...viewerMutation(draft), expected_generation: policy.generation })}
        >
          {save.isPending ? "Saving…" : "Save"}
        </Button>
      }
    >
      <div className="flex flex-col gap-4 pt-3">
        {limited && (
          <div className="flex flex-wrap items-center gap-3 text-sm">
            <p className="min-w-0 flex-1 text-muted-foreground">
              This public tunnel is still limited to you and the people listed here.
            </p>
            <Button variant="secondary" onClick={() => setConfirmOpen(true)} disabled={save.isPending}>
              Open to anyone
            </Button>
          </div>
        )}
        {teamAudience && (
          <p className="text-sm text-muted-foreground">A team can open this now. Saving replaces it with this list.</p>
        )}
        <ChipField
          label="Email addresses and domains"
          field={`viewers-${target.routeId}`}
          stateKey={`${key}:typed`}
          placeholder="name@example.com or *@example.com"
          hint={
            open
              ? "Anyone with the address can open it now. Add people to limit it to you and them."
              : teamOwned && !publicResource
                ? "They sign in with a verified email, and also need the team’s access. You can always open it."
                : "They sign in with a verified email. Leave empty so only you can open it."
          }
          values={draft}
          onChange={(values) => setDraft(addViewers([], values))}
        />
        {problem && <FieldError>{problem}</FieldError>}
        {save.isError && (
          <FieldError>
            <ErrorMessage error={save.error} />
          </FieldError>
        )}
      </div>
      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Open this tunnel to anyone?</AlertDialogTitle>
            <AlertDialogDescription>
              Anyone with the address can reach this service without signing in. Only the app’s own login protects it.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => save.mutate({ audience: "public", expected_generation: policy.generation })}
            >
              Open to anyone
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </Section>
  )
}
