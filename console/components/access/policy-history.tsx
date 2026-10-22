"use client"

import { useState } from "react"
import { useApiQuery } from "@/lib/api"
import {
  lineDiff,
  policyVersionQuery,
  policyVersionsQuery,
  restorePolicy,
  type Policy,
  type PolicyVersion,
} from "@/lib/policy"
import { cn } from "@/lib/utils"
import { ChangesDialog } from "@/components/access/changes-dialog"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"

const kinds: Record<PolicyVersion["kind"], string> = {
  saved: "Saved",
  restored: "Restored",
  rewritten: "Rewritten",
  migrated: "From earlier grants",
}

/** Every saved version, newest first; compare one with the current file and restore it. */
export function PolicyHistory({ workspace, policy }: { workspace: string; policy: Policy }) {
  const versions = useApiQuery(policyVersionsQuery(workspace))
  const [open, setOpen] = useState<number | null>(null)
  const [restoring, setRestoring] = useState<number | null>(null)
  if (versions.isPending) return <Skeleton className="h-40 w-full" />
  if (versions.isError) return <p className="text-sm text-destructive">{versions.error.message}</p>
  if (!versions.data.length) return <p className="text-sm text-muted-foreground">No versions yet.</p>
  return (
    <div className="flex flex-col border-t border-divider">
      {versions.data.map((version) => (
        <div key={version.version} className="flex flex-col border-b border-divider" data-version={version.version}>
          <div className="flex min-h-12 flex-wrap items-center gap-3 py-2 text-sm">
            <span className="w-12 font-medium tabular-nums">v{version.version}</span>
            <span className="min-w-0 flex-1">
              <span>{version.message || kinds[version.kind]}</span>
              <span className="block text-muted-foreground">
                <Time iso={version.created_at} relative /> · {version.author.replace(/^user:/, "")}
                {version.api_token && ` with API token ${version.api_token}`}
                {(version.gained > 0 || version.lost > 0) && ` · ${version.gained} gained, ${version.lost} lost`}
              </span>
            </span>
            {version.version === policy.version ? (
              <span className="text-muted-foreground">Current</span>
            ) : (
              <>
                <Button variant="outline" onClick={() => setOpen(open === version.version ? null : version.version)}>
                  {open === version.version ? "Hide diff" : "Diff"}
                </Button>
                {policy.can_edit === "all" && (
                  <Button variant="outline" onClick={() => setRestoring(version.version)}>
                    Restore
                  </Button>
                )}
              </>
            )}
          </div>
          {open === version.version && <VersionDiff workspace={workspace} version={version.version} current={policy} />}
        </div>
      ))}
      {restoring !== null && (
        <ChangesDialog
          workspace={workspace}
          title={`Restore version ${restoring}`}
          verb="Restore"
          run={(fields) =>
            restorePolicy(workspace, { version: restoring, expected_version: policy.version, ...fields })
          }
          onClose={() => setRestoring(null)}
          onDone={() => setRestoring(null)}
        />
      )}
    </div>
  )
}

function VersionDiff({ workspace, version, current }: { workspace: string; version: number; current: Policy }) {
  const old = useApiQuery(policyVersionQuery(workspace, version))
  if (old.isPending) return <Skeleton className="mb-3 h-24 w-full" />
  if (old.isError) return <p className="pb-3 text-sm text-destructive">{old.error.message}</p>
  const lines = lineDiff(old.data.policy, current.policy)
  return (
    <div className="mb-3 overflow-x-auto surface rounded-lg py-2 font-mono text-xs leading-5">
      <p className="px-3 pb-1 font-sans text-muted-foreground">
        v{version} → v{current.version} (current)
      </p>
      {lines.map((line, index) => (
        <div
          key={index}
          className={cn(
            "px-3 whitespace-pre",
            line.kind === "+" && "bg-success/10",
            line.kind === "-" && "bg-destructive/10",
            line.kind === " " && "text-muted-foreground",
          )}
        >
          {line.kind} {line.text}
        </div>
      ))}
    </div>
  )
}
