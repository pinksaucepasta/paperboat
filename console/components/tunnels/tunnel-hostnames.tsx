"use client"

import {
  CheckCircleIcon,
  CircleNotchIcon,
  PencilSimpleIcon,
  PlusIcon,
  PowerIcon,
  TrashIcon,
  WarningCircleIcon,
} from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { useApiQuery } from "@/lib/api"
import {
  addDomain,
  createRoute,
  deleteRoute,
  domainsQuery,
  hostOf,
  instructionsQuery,
  originLabel,
  patchRoute,
  removeDomain,
  removeRouteById,
  routeName,
  routesQuery,
  tunnelKeys,
  verifyDomain,
  type RouteInput,
  type Tunnel,
  type TunnelDomain,
  type TunnelRoute,
} from "@/lib/tunnels"
import { cn } from "@/lib/utils"
import { CopyText } from "@/components/copy-button"
import { ConfirmAction } from "@/components/list-toolbar"
import { RowMenus, type Entry } from "@/components/row-menus"
import { HostnameInput } from "@/components/domains/hostname-input"
import { Section } from "@/components/section"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

/*
 * Cloudflare Tunnel's model: a tunnel has public hostnames, each sending one hostname (and
 * optionally a path) to one service on the machine. The server keeps that as two things, a route
 * (hostname → service) and, for your own domain, a domain binding (DNS record and certificate).
 * Here they are one row: adding a hostname creates both, removing it removes both.
 */

/** The hostname a route answers, as people write it. */
function routeHost(route: TunnelRoute) {
  const match = route.host_match
  if (match.type === "catch_all") return "Any other hostname"
  if (match.type === "one_label_wildcard") return `*.${(match.hostname ?? "").replace(/^\*\./, "")}`
  return match.hostname ?? ""
}

/** A domain's setup, in the order it happens: DNS, then a certificate, then ready. */
export const domainStates: Record<TunnelDomain["state"], { label: string; tone: "ok" | "wait" | "bad" }> = {
  requested: { label: "Waiting for DNS", tone: "wait" },
  waiting_dns: { label: "Waiting for DNS", tone: "wait" },
  verified: { label: "Getting a certificate", tone: "wait" },
  issuing_tls: { label: "Getting a certificate", tone: "wait" },
  ready: { label: "Ready", tone: "ok" },
  conflict: { label: "Used by another tunnel", tone: "bad" },
  dns_error: { label: "DNS record is wrong", tone: "bad" },
  tls_error: { label: "Certificate failed", tone: "bad" },
  expired: { label: "Expired", tone: "bad" },
  quarantined: { label: "Blocked", tone: "bad" },
}

export const needsDns = (domain: { state: TunnelDomain["state"] }) =>
  domain.state === "requested" || domain.state === "waiting_dns" || domain.state === "dns_error"

export function TunnelHostnames({ tunnel }: { tunnel: Tunnel }) {
  const routes = useApiQuery(routesQuery(tunnel.id))
  const domains = useApiQuery(domainsQuery(tunnel.id))
  const [adding, setAdding] = useState(false)
  const bindingFor = (route: TunnelRoute) =>
    domains.data?.find((domain) => domain.route_id === route.id && domain.hostname === route.host_match.hostname)

  return (
    <Section
      title="Public hostnames"
      action={
        <Button variant="secondary" size="sm" onClick={() => setAdding(true)}>
          <PlusIcon data-icon="inline-start" />
          Add
        </Button>
      }
    >
      {routes.isPending ? (
        <div className="flex h-12 items-center border-b border-divider">
          <Skeleton className="h-4 w-1/3" />
        </div>
      ) : routes.isError ? (
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">
          Couldn’t load hostnames. {routes.error.message}
        </p>
      ) : routes.data.length === 0 ? (
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">
          Nothing is published yet. Add a hostname to send it to a service on the machine.
        </p>
      ) : (
        [...routes.data]
          .sort((a, b) => a.priority - b.priority)
          .map((route) => <HostnameRow key={route.id} route={route} domain={bindingFor(route)} tunnel={tunnel} />)
      )}
      {adding && <HostnameDialog tunnel={tunnel} onClose={() => setAdding(false)} />}
    </Section>
  )
}

function HostnameRow({ route, domain, tunnel }: { route: TunnelRoute; domain?: TunnelDomain; tunnel: Tunnel }) {
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const off = route.desired_state === "disabled"
  const host = routeHost(route)
  const refresh = async () => {
    await queryClient.invalidateQueries({ queryKey: tunnelKeys.routes(tunnel.id) })
    await queryClient.invalidateQueries({ queryKey: tunnelKeys.domains(tunnel.id) })
  }
  const toggle = useMutation({
    mutationFn: () => patchRoute(route, { desired_state: off ? "active" : "disabled" }),
    onSuccess: refresh,
    onError: (error) => toast.error("Couldn’t change the hostname", { description: error.message }),
  })
  const verify = useMutation({
    mutationFn: () => verifyDomain(domain!),
    onSuccess: refresh,
    onError: (error) => toast.error("Couldn’t check the DNS record", { description: error.message }),
  })
  const state = domain && domainStates[domain.state]

  const entries: Entry[] = [
    { kind: "item", label: "Edit…", icon: PencilSimpleIcon, onSelect: () => setEditing(true) },
    { kind: "item", label: off ? "Turn on" : "Turn off", icon: PowerIcon, onSelect: () => toggle.mutate() },
    { kind: "separator" },
    { kind: "item", label: "Remove hostname", icon: TrashIcon, destructive: true, onSelect: () => setDeleting(true) },
  ]

  return (
    <RowMenus
      label={`Actions for ${host}${route.path_prefix ?? ""}`}
      entries={entries}
      row={(actions) => (
        <div className="flex flex-col gap-1 border-b border-divider py-2 text-sm data-popup-open:bg-surface">
          {/* A fixed grid, so the arrows and services line up down the list whatever the hostnames. */}
          <div className="grid min-h-8 grid-cols-[minmax(0,1fr)_auto] items-center gap-4 sm:grid-cols-[minmax(0,1fr)_1rem_minmax(0,1fr)_10rem]">
            <span className="flex h-[1lh] min-w-0 items-center gap-2">
              <span className={cn("min-w-0 truncate-x font-mono text-code trim-cap", off && "text-muted-foreground")}>
                {host}
                {route.path_prefix && <span className="text-muted-foreground">{route.path_prefix}</span>}
              </span>
              {off && (
                <Badge variant="outline" className="font-normal text-muted-foreground trim-cap">
                  Off
                </Badge>
              )}
            </span>
            <span className="hidden text-center text-muted-foreground trim-cap sm:block">→</span>
            <span className="hidden min-w-0 truncate-x font-mono text-code text-muted-foreground trim-cap sm:block">
              {originLabel(route.origin)}
            </span>
            <span className="flex items-center justify-end gap-3">
              {state && state.tone !== "ok" && (
                <span
                  className={cn(
                    "flex shrink-0 items-center gap-1 text-xs",
                    state.tone === "bad" ? "text-destructive" : "text-muted-foreground",
                  )}
                >
                  {verify.isPending ? (
                    <CircleNotchIcon className="size-3.5 animate-spin" />
                  ) : state.tone === "bad" ? (
                    <WarningCircleIcon weight="fill" className="size-3.5" />
                  ) : null}
                  <span className="trim-cap">{state.label}</span>
                </span>
              )}
              {state?.tone === "ok" && (
                <CheckCircleIcon weight="fill" className="size-3.5 text-success" aria-label="Ready" />
              )}
              {actions}
            </span>
          </div>
          {domain && needsDns(domain) && (
            <DnsRecord domain={domain} onCheck={() => verify.mutate()} checking={verify.isPending} />
          )}
        </div>
      )}
      dialogs={
        <>
          {editing && (
            <HostnameDialog tunnel={tunnel} route={route} domain={domain} onClose={() => setEditing(false)} />
          )}
          <ConfirmAction
            open={deleting}
            onOpenChange={setDeleting}
            title={`Remove ${host}${route.path_prefix ?? ""}?`}
            description={
              domain
                ? "It stops reaching this tunnel. The DNS record stays at your DNS provider; remove it there."
                : `It stops sending traffic to ${originLabel(route.origin)}.`
            }
            action="Remove hostname"
            pending="Removing…"
            mutation={{
              mutationFn: async () => {
                if (domain) await removeDomain(domain)
                await deleteRoute(route)
              },
              onSuccess: refresh,
            }}
          />
        </>
      }
    />
  )
}

/** The record to add at the DNS provider, each part copied on click, then "Check again". */
export function DnsRecord({
  domain,
  onCheck,
  checking,
}: {
  domain: TunnelDomain
  onCheck: () => void
  checking: boolean
}) {
  const instructions = useApiQuery(instructionsQuery(domain.tunnel_id, domain.id))
  const records = instructions.data?.records ?? [
    { type: "CNAME" as const, name: domain.hostname, value: domain.dns.target, ttl: 300 },
  ]
  return (
    <div className="flex flex-col gap-0.5 text-xs text-muted-foreground">
      {records.map((record) => (
        <div key={`${record.name}-${record.value}`} className="flex flex-wrap items-center gap-x-2">
          Add record
          <span className="font-mono text-foreground">{record.type}</span>
          <CopyText value={record.name} className="font-mono text-foreground" />→
          <CopyText value={record.value} className="font-mono text-foreground" />
        </div>
      ))}
      <div className="flex flex-wrap items-center gap-x-2">
        {domain.dns.observed_records?.length ? <span>Now {domain.dns.observed_records.join(", ")}.</span> : null}
        <button
          type="button"
          onClick={onCheck}
          disabled={checking}
          className="underline-offset-4 outline-none hover:text-foreground hover:underline focus-visible:underline"
        >
          Check again
        </button>
      </div>
    </div>
  )
}

/**
 * Reads what people type for a service: `3000`, `localhost:3000`, or a URL such as
 * `https://localhost:8443`, `tcp://localhost:5432` or `unix:/run/app.sock`.
 */
export function parseService(value: string): RouteInput["origin"] | undefined {
  const text = value.trim()
  if (/^\d{1,5}$/.test(text)) return { scheme: "http", address: `localhost:${text}`, preserve_host: false }
  const unix = /^unix:(\/.+)$/.exec(text)
  if (unix) return { scheme: "unix", address: unix[1], preserve_host: false }
  const url = /^(https?|h2c|tcp):\/\/([^/\s]+)\/?$/.exec(text)
  const [scheme, address] = url ? [url[1], url[2]] : ["http", text]
  if (!/^[a-z0-9.-]+:\d{1,5}$|^\[[0-9a-f:]+\]:\d{1,5}$/i.test(address)) return
  return { scheme: scheme as RouteInput["origin"]["scheme"], address, preserve_host: false }
}

const serviceText = (origin: TunnelRoute["origin"]) =>
  origin.scheme === "unix"
    ? `unix:${origin.address}`
    : origin.scheme === "http"
      ? origin.address.replace(/^127\.0\.0\.1(?=:)/, "localhost")
      : `${origin.scheme}://${origin.address}`

/**
 * Add or edit a public hostname, Cloudflare-style: which hostname (the tunnel's own address, or
 * your domain, wildcards included) and optional path go to which service on the machine.
 */
function HostnameDialog({
  tunnel,
  route,
  domain: binding,
  onClose,
}: {
  tunnel: Tunnel
  route?: TunnelRoute
  domain?: TunnelDomain
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const managed = hostOf(tunnel.stable_endpoint)
  const startOwn = route ? route.host_match.type !== "managed_exact" : false
  const [own, setOwn] = useState(startOwn)
  const [hostname, setHostname] = useState(startOwn && route ? routeHost(route) : "")
  const [path, setPath] = useState(route?.path_prefix ?? "")
  const [service, setService] = useState(route ? serviceText(route.origin) : "")

  const origin = parseService(service)
  const domain = hostname.trim().toLowerCase().replace(/\.$/, "")
  const pathValue = path.trim() ? (path.trim().startsWith("/") ? path.trim() : `/${path.trim()}`) : null
  const hostMatch: RouteInput["host_match"] = !own
    ? { type: "managed_exact", hostname: managed }
    : domain.startsWith("*.")
      ? { type: "one_label_wildcard", hostname: domain, wildcard_labels: 1 }
      : { type: "exact", hostname: domain }
  const problems = {
    domain:
      own && !/^(\*\.)?([a-z0-9-]+\.)+[a-z]{2,}$/.test(domain)
        ? "Enter a domain like app.example.com, or *.example.com for all its subdomains."
        : undefined,
    service: service.trim() && !origin ? "Use a port, host:port, or a URL like https://localhost:8443." : undefined,
  }
  const ready = Boolean(origin) && !problems.domain

  const save = useMutation({
    mutationFn: async () => {
      const input: RouteInput = {
        name: route?.name ?? routeName(own ? domain : null, pathValue),
        host_match: hostMatch,
        path_prefix: pathValue,
        origin: origin!,
      }
      const routeId = route ? (await patchRoute(route, input), route.id) : await createRoute(tunnel.id, input)
      // Your own domain needs its DNS record and certificate; the tunnel's address has both.
      const keep = own && binding?.hostname === domain
      if (binding && !keep) await removeDomain(binding)
      if (own && !keep) {
        try {
          await addDomain(tunnel.id, domain, routeId)
        } catch (error) {
          // A route that was only just created would stay listed without DNS or a certificate.
          if (!route) await removeRouteById(tunnel.id, routeId).catch(() => {})
          throw error
        }
      }
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: tunnelKeys.routes(tunnel.id) })
      await queryClient.invalidateQueries({ queryKey: tunnelKeys.domains(tunnel.id) })
      onClose()
    },
  })

  return (
    <Dialog open onOpenChange={(next) => !next && !save.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{route ? "Edit public hostname" : "Add a public hostname"}</DialogTitle>
        </DialogHeader>
        <form
          className="flex min-w-0 flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (ready) save.mutate()
          }}
        >
          <Field className="min-w-0 gap-1.5">
            <FieldLabel>Hostname</FieldLabel>
            <ToggleGroup
              value={[own ? "own" : "managed"]}
              onValueChange={(value) => value[0] && setOwn(value[0] === "own")}
              variant="outline"
              spacing={0}
              className="w-full"
            >
              <ToggleGroupItem value="managed" className="h-8 flex-1 text-sm">
                Tunnel address
              </ToggleGroupItem>
              <ToggleGroupItem value="own" className="h-8 flex-1 text-sm">
                Your domain
              </ToggleGroupItem>
            </ToggleGroup>
            {own ? (
              <>
                <HostnameInput
                  autoFocus
                  value={hostname}
                  onChange={setHostname}
                  invalid={Boolean(hostname && problems.domain)}
                />
                {hostname && problems.domain ? (
                  <FieldError>{problems.domain}</FieldError>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    Use * for every subdomain. You’ll add one DNS record for it.
                  </p>
                )}
              </>
            ) : (
              <p className="truncate font-mono text-xs text-muted-foreground">{managed}</p>
            )}
          </Field>
          <Field className="gap-1.5">
            <FieldLabel htmlFor="hostname-path" className="flex items-baseline justify-between">
              Path
              <span className="text-xs font-normal text-muted-foreground">Optional</span>
            </FieldLabel>
            <Input
              id="hostname-path"
              placeholder="/api"
              value={path}
              onChange={(event) => setPath(event.target.value.replace(/\s/g, ""))}
              className="font-mono"
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field data-invalid={Boolean(problems.service)} className="gap-1.5">
            <FieldLabel htmlFor="hostname-service">Service</FieldLabel>
            <Input
              id="hostname-service"
              autoFocus={!own}
              placeholder="localhost:3000"
              value={service}
              onChange={(event) => setService(event.target.value)}
              aria-invalid={Boolean(problems.service)}
              className="font-mono"
              autoComplete="off"
              spellCheck={false}
            />
            {problems.service ? (
              <FieldError>{problems.service}</FieldError>
            ) : (
              <p className="text-xs text-muted-foreground">On the tunnel’s machine. A port works too.</p>
            )}
          </Field>
          {save.isError && <p className="text-sm text-destructive">{save.error.message}</p>}
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!ready || save.isPending}>
              {save.isPending ? "Saving…" : route ? "Save" : "Add hostname"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
