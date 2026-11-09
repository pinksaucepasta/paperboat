"use client"

import { ShieldCheckIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { useApiQuery, useWorkspace } from "@/lib/api"
import { notOnServer, PolicyError, policyQuery } from "@/lib/policy"
import { cn } from "@/lib/utils"
import { PolicyEditor } from "@/components/access/policy-editor"
import { PolicyHistory } from "@/components/access/policy-history"
import { PolicyTest } from "@/components/access/policy-test"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"

const tabs = [
  { id: "file", title: "Policy file" },
  { id: "test", title: "Test" },
  { id: "history", title: "History" },
] as const

export type AccessTab = (typeof tabs)[number]["id"]

/** Workspace → Access: one policy per workspace: the file, a tester and its history. */
export function AccessPage() {
  const workspace = useWorkspace()
  const policy = useApiQuery(policyQuery(workspace?.id ?? ""))
  const [tab, setTab] = useState<AccessTab>("file")
  // The unsaved file lives here so Test can try it and switching tabs keeps it.
  const [draft, setDraft] = useState<{ base: number; text: string } | null>(null)

  if (policy.isError) return <PolicyUnavailable error={policy.error} />
  const data = policy.data
  const text = data && draft && draft.base === data.version ? draft.text : data?.policy

  return (
    <div className="flex flex-col gap-6">
      <header>
        <div className="border-b border-divider">
          <nav className="-mb-px no-scrollbar flex gap-1 overflow-x-auto" role="tablist">
            {tabs.map(({ id, title }) => (
              <button
                key={id}
                type="button"
                role="tab"
                aria-selected={tab === id}
                onClick={() => setTab(id)}
                className={cn(
                  "shrink-0 border-b-2 border-transparent px-2.5 pb-2.5 text-sm text-muted-foreground transition-colors hover:text-foreground",
                  tab === id && "border-foreground text-foreground",
                )}
              >
                {title}
                {id === "file" && data && text !== data.policy && (
                  <span className="ml-1 text-muted-foreground">•</span>
                )}
              </button>
            ))}
          </nav>
        </div>
      </header>
      {!workspace || !data || text === undefined ? (
        <Skeleton className="h-[32rem] rounded-lg" />
      ) : tab === "file" ? (
        <PolicyEditor
          workspace={workspace.id}
          policy={data}
          text={text}
          onChange={(next) => setDraft({ base: data.version, text: next })}
          onSaved={() => setDraft(null)}
        />
      ) : tab === "test" ? (
        <PolicyTest workspace={workspace.id} policy={data} draft={text !== data.policy ? text : undefined} />
      ) : (
        <PolicyHistory workspace={workspace.id} policy={data} />
      )}
    </div>
  )
}

function PolicyUnavailable({ error }: { error: Error }) {
  const missing = error instanceof PolicyError && error.code === notOnServer
  const forbidden = error instanceof PolicyError && error.status === 403
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <ShieldCheckIcon />
        </EmptyMedia>
        <EmptyTitle>
          {missing
            ? "Access policy isn’t available on this server yet"
            : forbidden
              ? "No access"
              : "Couldn’t load access"}
        </EmptyTitle>
        {!missing && <EmptyDescription>{error.message}</EmptyDescription>}
      </EmptyHeader>
    </Empty>
  )
}
