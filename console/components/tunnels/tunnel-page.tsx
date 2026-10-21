"use client"

import { ArrowSquareOutIcon, CaretDownIcon, PauseIcon, PlayIcon, WarningCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { useState } from "react"
import { toast } from "sonner"
import { useApiQuery, useMachines } from "@/lib/api"
import { hostOf, patchTunnel, tunnelHealthQuery, tunnelQuery, type Tunnel } from "@/lib/tunnels"
import { CopyRow } from "@/components/copy-button"
import { Detail } from "@/components/machine-overview"
import { CrumbLink, Crumb, PageHeader } from "@/components/page-header"
import { Time } from "@/components/time"
import { useTunnelActions } from "@/components/tunnels/tunnel-actions"
import { TunnelTitle } from "@/components/tunnels/tunnel-rename"
import { TunnelHostnames } from "@/components/tunnels/tunnel-hostnames"
import { TunnelStatus, tunnelState } from "@/components/tunnels/tunnel-status"
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Skeleton } from "@/components/ui/skeleton"

/**
 * A tunnel's page, laid out like a machine's Overview: who and where it serves, the hostnames
 * it publishes (Cloudflare's "public hostnames"), and deletion.
 */
export function TunnelPage({ id }: { id: string }) {
  const query = useApiQuery(tunnelQuery(id))
  const tunnel = query.data

  return (
    <>
      <PageHeader>
        <CrumbLink href="/tunnels">Tunnels</CrumbLink>
        {tunnel ? <Crumb>{tunnel.name}</Crumb> : <Skeleton className="h-4 w-24" />}
      </PageHeader>
      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col gap-10 px-4 py-6 md:px-8 md:py-8">
        {query.isError ? (
          <div className="flex flex-col items-center gap-4 py-24 text-center text-sm text-muted-foreground">
            {query.error.message}
            <Button variant="outline" size="sm" render={<Link href="/tunnels" />} nativeButton={false}>
              All tunnels
            </Button>
          </div>
        ) : !tunnel ? (
          <div className="flex flex-col gap-3">
            <Skeleton className="h-8 w-48" />
            <Skeleton className="h-4 w-72" />
          </div>
        ) : (
          <Loaded tunnel={tunnel} />
        )}
      </main>
    </>
  )
}

function Loaded({ tunnel }: { tunnel: Tunnel }) {
  const router = useRouter()
  const { toggle, paused, dialogs, openDelete } = useTunnelActions(tunnel, { onDeleted: () => router.push("/tunnels") })
  const machines = useMachines()
  const machine = machines.data?.find((entry) => entry.id === tunnel.created_by_host_id)

  return (
    <>
      <div className="flex flex-wrap items-start gap-4">
        <div className="flex min-w-0 flex-1 flex-col gap-2">
          <TunnelTitle tunnel={tunnel} />
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted-foreground">
            <TunnelStatus tunnel={tunnel} />
            <a
              href={tunnel.stable_endpoint}
              target="_blank"
              rel="noopener"
              className="font-mono text-code underline-offset-4 hover:text-foreground hover:underline"
            >
              {hostOf(tunnel.stable_endpoint)}
            </a>
          </div>
        </div>
        <Button variant="secondary" size="lg" onClick={() => toggle.mutate()} disabled={toggle.isPending}>
          {paused ? <PlayIcon data-icon="inline-start" /> : <PauseIcon data-icon="inline-start" />}
          {paused ? "Resume" : "Pause"}
        </Button>
        <Button
          size="lg"
          render={<a href={tunnel.stable_endpoint} target="_blank" rel="noopener" />}
          nativeButton={false}
        >
          <ArrowSquareOutIcon data-icon="inline-start" />
          Open
        </Button>
      </div>

      <HealthNotice tunnel={tunnel} />

      <section className="flex flex-col gap-3">
        <h2 className="text-sm font-medium">Details</h2>
        <dl className="grid border-t border-divider sm:grid-cols-2 sm:gap-x-10">
          <Detail label="Address" control>
            <AddressValue tunnel={tunnel} />
          </Detail>
          <Detail label="Machine">
            {machine ? (
              <Link href={`/machines/${machine.id}/tunnels`} className="underline-offset-4 hover:underline">
                {machine.alias}
              </Link>
            ) : undefined}
          </Detail>
          <Detail label="Who can open it" control>
            <AccessValue tunnel={tunnel} />
          </Detail>
          <Detail label="Created">
            <Time iso={tunnel.created_at} />
          </Detail>
          <Detail label="Tunnel ID" mono copy={tunnel.id}>
            {tunnel.id}
          </Detail>
          <Detail label="Expires">{tunnel.expires_at ? <Time iso={tunnel.expires_at} /> : "Never"}</Detail>
        </dl>
      </section>

      <TunnelHostnames tunnel={tunnel} />

      <section className="flex flex-col gap-3">
        <h2 className="text-sm font-medium text-destructive">Danger zone</h2>
        <div className="flex flex-wrap items-center gap-4 rounded-lg bg-destructive/5 p-4 dark:bg-destructive/10">
          <div className="flex min-w-0 flex-1 flex-col gap-0.5">
            <p className="text-sm font-medium">Delete tunnel</p>
            <p className="text-sm text-muted-foreground">Its address and public hostnames stop working for good.</p>
          </div>
          <Button
            variant="secondary"
            size="lg"
            className="bg-background text-destructive hover:bg-background/70 dark:bg-white/10 dark:hover:bg-white/15"
            onClick={openDelete}
          >
            Delete
          </Button>
        </div>
      </section>
      {dialogs}
    </>
  )
}

/**
 * When a running tunnel isn't healthy, the server's own explanation and the fix it suggests.
 * Paused is a choice, not a problem, so it gets no notice.
 */
function HealthNotice({ tunnel }: { tunnel: Tunnel }) {
  const problem = tunnel.desired_state === "active" && !tunnelState(tunnel).healthy
  const health = useApiQuery({ ...tunnelHealthQuery(tunnel.id), enabled: problem })
  if (!problem || !health.data || health.data.overall_code === "ready") return null
  return (
    <div className="flex items-start gap-3 rounded-lg bg-surface p-4 text-sm">
      <WarningCircleIcon weight="fill" className="mt-0.5 size-4 shrink-0 text-destructive" />
      <div className="flex min-w-0 flex-col gap-0.5">
        <p className="font-medium">{health.data.summary}</p>
        {health.data.repair_action && <p className="text-muted-foreground">{health.data.repair_action}</p>}
        {health.data.overall_code === "bandwidth_exhausted" && (
          <p className="flex gap-3 pt-1">
            <Link href="/billing" className="font-medium underline-offset-4 hover:underline">
              Add bandwidth
            </Link>
            <Link href="/billing" className="font-medium underline-offset-4 hover:underline">
              Change plan
            </Link>
          </p>
        )}
      </div>
    </div>
  )
}

/** The managed address, copied on click. It is generated; only the tunnel's name changes. */
function AddressValue({ tunnel }: { tunnel: Tunnel }) {
  const host = hostOf(tunnel.stable_endpoint)
  return (
    <CopyRow value={host} className="-ml-2.5 min-w-0 font-mono text-code">
      {host}
    </CopyRow>
  )
}

const audiences = { public: "Anyone with the address", private: "Only you" } as const

/** Who can open the tunnel, changed from a caret menu like the machine's Keep awake. */
function AccessValue({ tunnel }: { tunnel: Tunnel }) {
  const queryClient = useQueryClient()
  const [confirmPublic, setConfirmPublic] = useState(false)
  const save = useMutation({
    mutationFn: (mode: Tunnel["access_mode"]) => patchTunnel(tunnel, { access_mode: mode }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["tunnel", tunnel.id] })
      await queryClient.invalidateQueries({ queryKey: ["tunnels"] })
    },
    onError: (error) => toast.error("Couldn’t change who can open it", { description: error.message }),
  })
  const choose = (mode: string) => {
    if (mode === tunnel.access_mode) return
    if (mode === "public") setConfirmPublic(true)
    else save.mutate("private")
  }
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger
          disabled={save.isPending}
          className="inline-flex items-center gap-1 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
        >
          {audiences[tunnel.access_mode]}
          <CaretDownIcon className="size-3.5 text-muted-foreground" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="w-60">
          <DropdownMenuRadioGroup value={tunnel.access_mode} onValueChange={choose}>
            <DropdownMenuRadioItem value="public">{audiences.public}</DropdownMenuRadioItem>
            <DropdownMenuRadioItem value="private">{audiences.private}, after signing in</DropdownMenuRadioItem>
          </DropdownMenuRadioGroup>
        </DropdownMenuContent>
      </DropdownMenu>
      <AlertDialog open={confirmPublic} onOpenChange={setConfirmPublic}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Make {tunnel.name} public?</AlertDialogTitle>
            <AlertDialogDescription>
              Anyone who has {hostOf(tunnel.stable_endpoint)} can reach the service without signing in. Its own login,
              if it has one, is the only protection.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => save.mutate("public")}>Make public</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}
