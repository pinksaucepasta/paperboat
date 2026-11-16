"use client"

import { ArrowSquareOutIcon, MagnifyingGlassIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useApiQuery, useMachines } from "@/lib/api"
import { hostOf, originLabel, previewPoll, previewQuery, type Preview } from "@/lib/tunnels"
import { CopyRow } from "@/components/copy-button"
import { Detail } from "@/components/machine-overview"
import { CrumbLink, Crumb, PageHeader } from "@/components/page-header"
import { Section, SectionHeading } from "@/components/section"
import { Time } from "@/components/time"
import { ended, PreviewStatus } from "@/components/tunnels/preview-list"
import { IpRules } from "@/components/tunnels/ip-rules"
import { ResourceHistory } from "@/components/tunnels/resource-history"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { useNow } from "@/lib/use-now"

/** A preview's page: whether it answers, where it is and where it forwards to, its domains and IP rules. */
export function PreviewPage({ id }: { id: string }) {
  const query = useApiQuery({ ...previewQuery(id), refetchInterval: previewPoll(10_000) })
  const preview = query.data
  return (
    <>
      <PageHeader>
        <CrumbLink href="/previews">Previews</CrumbLink>
        {preview ? <Crumb>{hostOf(preview.endpoint) || "Preview"}</Crumb> : <Skeleton className="my-0.5 h-4 w-24" />}
      </PageHeader>
      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col gap-10 px-4 py-6 md:px-8 md:py-8">
        {query.isError ? (
          <div className="flex flex-col items-center gap-4 py-24 text-center text-sm text-muted-foreground">
            {query.error.message}
            <Button variant="outline" size="sm" render={<Link href="/previews" />} nativeButton={false}>
              All previews
            </Button>
          </div>
        ) : !preview ? (
          <Skeleton className="h-40 w-full" />
        ) : (
          <Loaded preview={preview} />
        )}
      </main>
    </>
  )
}

const domainStates: Record<string, string> = {
  ready: "Ready",
  conflict: "Needs attention",
  dns_error: "Needs attention",
  tls_error: "Needs attention",
  expired: "Expired",
  quarantined: "Blocked",
}

function Loaded({ preview }: { preview: Preview }) {
  const now = useNow(30_000)
  const machines = useMachines()
  const machine = machines.data?.find((entry) => entry.id === preview.owner_machine_id)
  const over = ended(preview, now)
  const host = hostOf(preview.endpoint)
  return (
    <>
      <div className="flex flex-wrap items-start gap-4">
        <div className="flex min-w-0 flex-1 flex-col gap-2">
          <h1 className="truncate font-mono text-xl font-semibold">{host || "Preview"}</h1>
          <div className="text-sm text-muted-foreground">
            {over ? "Ended" : <PreviewStatus preview={preview} />}
          </div>
        </div>
        {!over && (
          <>
            <Button
              variant="secondary"
              size="lg"
              render={<Link href={`/previews/${encodeURIComponent(preview.id)}/inspect`} />}
              nativeButton={false}
            >
              <MagnifyingGlassIcon data-icon="inline-start" />
              Inspect
            </Button>
            <Button size="lg" render={<a href={preview.endpoint} target="_blank" rel="noopener" />} nativeButton={false}>
              <ArrowSquareOutIcon data-icon="inline-start" />
              Open
            </Button>
          </>
        )}
      </div>

      <section className="flex flex-col gap-3">
        <SectionHeading title="Details" />
        <dl className="grid border-t border-divider sm:grid-cols-2 sm:gap-x-10">
          <Detail label="Address" control>
            {host && (
              <CopyRow value={host} className="-ml-2.5 min-w-0 font-mono text-code">
                {host}
              </CopyRow>
            )}
          </Detail>
          <Detail label="Machine">
            {machine ? (
              <Link href={`/machines/${machine.id}/previews`} className="underline-offset-4 hover:underline">
                {machine.alias}
              </Link>
            ) : undefined}
          </Detail>
          <Detail label="Forwards to" mono>
            {originLabel(preview.target)}
          </Detail>
          <Detail label="Who can open it">
            {preview.access_mode === "private" ? "Only you, after signing in" : "Anyone with the address"}
          </Detail>
          <Detail label="Created">{preview.created_at ? <Time iso={preview.created_at} /> : undefined}</Detail>
          <Detail label={over ? "Ended" : "Ends"}>
            {over ? (
              <Time iso={preview.last_renewed_at ?? preview.user_deadline ?? preview.lease_deadline} />
            ) : preview.user_deadline ? (
              <Time iso={preview.user_deadline} />
            ) : preview.owner_session_kind === "foreground" ? (
              "When stopped"
            ) : (
              <Time iso={preview.lease_deadline} />
            )}
          </Detail>
          <Detail label="Preview ID" mono copy={preview.id}>
            {preview.id}
          </Detail>
        </dl>
      </section>

      <Section title="Domains">
        {preview.domains.length ? (
          preview.domains.map((domain) => (
            <div
              key={domain.id}
              className="flex min-h-12 items-center justify-between gap-4 border-b border-divider text-sm"
            >
              <span className="min-w-0 truncate font-mono text-code">{domain.hostname}</span>
              <span className="shrink-0 text-muted-foreground">{domainStates[domain.state] ?? "Setting up"}</span>
            </div>
          ))
        ) : (
          <p className="border-b border-divider py-3 text-sm text-muted-foreground">None besides its address.</p>
        )}
      </Section>

      <IpRules kind="preview" id={preview.id} />

      <ResourceHistory kind="preview" id={preview.id} />
    </>
  )
}
