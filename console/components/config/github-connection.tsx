"use client"

import { GithubLogoIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useApiQuery } from "@/lib/api"
import { disconnectGitHub, githubLive, githubStatusQuery, startConnect } from "@/lib/github"
import { ConfirmAction } from "@/components/list-toolbar"
import { Section } from "@/components/section"
import { Avatar, AvatarFallback, AvatarImage } from "@/components/ui/avatar"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"

/**
 * Your GitHub connection, which config sync uses to read and write your config repository:
 * connect (GitHub returns to /github/done and back here) or disconnect.
 */
export function GitHubConnection() {
  const queryClient = useQueryClient()
  const status = useApiQuery(githubStatusQuery)
  const [disconnecting, setDisconnecting] = useState(false)
  const connect = useMutation({
    mutationFn: () => startConnect("/config"),
    onSuccess: ({ authorization_url }) => window.location.assign(authorization_url),
  })
  if (!githubLive) return null
  if (status.isPending)
    return (
      // The connected row (the usual case), its words as they are and the account and repository as
      // placeholders; on a phone it wraps like the loaded line.
      <Section
        title="GitHub"
        action={
          <Button variant="outline" size="lg" tabIndex={-1} aria-hidden className="pointer-events-none">
            Disconnect
          </Button>
        }
      >
        <div className="flex min-h-12 items-center gap-3 border-b border-divider py-2 text-sm">
          <Skeleton className="size-6 shrink-0 rounded-full" />
          <span>
            Connected as <Skeleton className="inline-block h-3 w-20 align-middle" />
            <span className="text-muted-foreground">
              {" "}
              · config in <Skeleton className="inline-block h-3 w-40 align-middle" />
            </span>
          </span>
        </div>
      </Section>
    )
  if (status.isError) return <p className="text-sm text-muted-foreground">{status.error.message}</p>
  const data = status.data
  return (
    <Section
      title="GitHub"
      action={
        data.connected ? (
          <Button variant="outline" size="lg" onClick={() => setDisconnecting(true)}>
            Disconnect
          </Button>
        ) : (
          <Button size="lg" disabled={connect.isPending} onClick={() => connect.mutate()}>
            <GithubLogoIcon data-icon="inline-start" weight="fill" />
            {connect.isPending ? "Opening GitHub…" : "Connect GitHub"}
          </Button>
        )
      }
    >
      <div className="flex min-h-12 items-center gap-3 border-b border-divider py-2 text-sm" data-github={data.connected ? "connected" : "disconnected"}>
        {data.connected ? (
          <>
            <Avatar className="size-6">
              {data.avatar_url && <AvatarImage src={data.avatar_url} alt="" />}
              <AvatarFallback>{data.login?.slice(0, 1).toUpperCase()}</AvatarFallback>
            </Avatar>
            <span>
              Connected as <span className="font-medium">{data.login}</span>
              {data.config_repo_name && (
                <span className="text-muted-foreground">
                  {" "}
                  · config in {data.config_repo_owner}/{data.config_repo_name}
                </span>
              )}
            </span>
          </>
        ) : (
          <span className="text-muted-foreground">
            Connect GitHub to keep your config files in a private repository.
          </span>
        )}
      </div>
      {data.missing_scopes.length > 0 && (
        <p className="py-2 text-sm text-destructive">GitHub is missing access Paperboat needs; connect again to grant it.</p>
      )}
      {connect.isError && <p className="py-2 text-sm text-destructive">{connect.error.message}</p>}
      <ConfirmAction
        open={disconnecting}
        onOpenChange={setDisconnecting}
        title="Disconnect GitHub?"
        description="Machines stop syncing until you reconnect GitHub. Your repository stays. You can also revoke Paperboat in GitHub → Settings → Applications."
        action="Disconnect"
        pending="Disconnecting…"
        mutation={{
          mutationFn: () => disconnectGitHub(),
          onSuccess: (next) => queryClient.setQueryData(githubStatusQuery.queryKey, next),
        }}
      />
    </Section>
  )
}
