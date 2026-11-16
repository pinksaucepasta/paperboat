"use client"

import { useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useApiQuery } from "@/lib/api"
import { cliSessionsPageSize, cliSessionsQuery, signOutCliSession, type CliSession } from "@/lib/cli-sessions"
import { usePageParam } from "@/lib/use-page-param"
import { ListPager } from "@/components/list-pager"
import { LoadError } from "@/components/load-error"
import { ConfirmAction } from "@/components/list-toolbar"
import { Section } from "@/components/section"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"

const kinds = { desktop: "Computer", server: "Server", container: "Container" }

/** The command-line sign-ins on this account, each one ended from here. */
export function CliSessions() {
  const pages = usePageParam("cli-sessions")
  const query = useApiQuery(cliSessionsQuery(pages.page))
  const [ending, setEnding] = useState<CliSession | null>(null)
  const queryClient = useQueryClient()
  const sessions = query.data?.items ?? []
  return (
    <Section title="Signed-in command lines">
      {query.isError ? (
        <LoadError what="sign-ins" error={query.error} retry={() => query.refetch()} />
      ) : !query.data ? (
        Array.from({ length: 2 }, (_, index) => (
          <div key={index} className="flex min-h-14 items-center justify-between gap-4 border-b border-divider">
            <Skeleton className="h-4 w-40" />
            <Skeleton className="h-7 w-20" />
          </div>
        ))
      ) : sessions.length === 0 ? (
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">No command lines are signed in.</p>
      ) : (
        sessions.map((session) => (
          <div
            key={session.cli_client_session_id}
            className="flex min-h-14 items-center justify-between gap-4 border-b border-divider text-sm"
          >
            <div className="flex min-w-0 flex-col">
              <span className="truncate font-medium">{session.client_label}</span>
              <span className="truncate text-muted-foreground">
                {kinds[session.device_type]} · {session.os} ·{" "}
                {session.last_used_at ? (
                  <>
                    used <Time iso={session.last_used_at} relative />
                  </>
                ) : (
                  <>
                    signed in <Time iso={session.created_at} relative />
                  </>
                )}
              </span>
            </div>
            <Button variant="outline" size="sm" onClick={() => setEnding(session)}>
              Sign out
            </Button>
          </div>
        ))
      )}
      {query.data && (
        <ListPager page={pages.page} size={cliSessionsPageSize} total={query.data.pagination.total} go={pages.go} />
      )}
      {ending && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setEnding(null)}
          title={`Sign out ${ending.client_label}?`}
          description="It has to sign in again before it can use your account."
          action="Sign out"
          pending="Signing out…"
          mutation={{
            mutationFn: () => signOutCliSession(ending.cli_client_session_id),
            onSuccess: () => queryClient.invalidateQueries({ queryKey: ["cli-client-sessions"] }),
          }}
        />
      )}
    </Section>
  )
}
