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
  if (status.isPending) return <Skeleton className="h-16 w-full" />
  if (status.isError) return <p className="text-sm text-muted-foreground">{status.error.message}</p>
  const data = status.data
  return (
    <Section
      title="GitHub"
      action={
        data.connected ? (
          <Button variant="outline" onClick={() => setDisconnecting(true)}>
            Disconnect
          </Button>
        ) : (
          <Button disabled={connect.isPending} onClick={() => connect.mutate()}>
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
        description="Machines stop syncing config until GitHub is connected again. Your repository stays on GitHub, and Paperboat still appears under Applications in your GitHub settings, where you can revoke it too."
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
