import type { Data } from "@/lib/api"

export type Level = "normal" | "warning" | "critical"
export type HostResource = { percent: number; level: Level }
export type HostsPayload = { hosts: Record<string, { resources: Record<string, HostResource>; unreachable: boolean }>; stale_after_seconds: number }
export type CanaryStep = { step: string; ok: boolean; error: string }
export type CanaryRun = { started: string; finished: string; ok: boolean; steps: CanaryStep[] }
export type CanaryPayload = { red: boolean; red_since: string | null; failing: Record<string, number>; last: CanaryRun | null; account_last: CanaryRun | null; stale_after_seconds: number }

export type Reports = Data<"/v1/admin/host-health">["reports"]
export type Alert = { id: string; level: "warning" | "critical"; title: string; body: string; since?: string }

/** The report of one source, typed. The server stores whatever the reporter sent, so a payload that is not the expected shape reads as absent. */
export function pick<T>(reports: Reports | undefined, source: "hosts" | "canary"): { payload: T; reported_at: string } | undefined {
  const found = reports?.find((r) => r.source === source)
  return found && typeof found.payload === "object" ? { payload: found.payload as T, reported_at: found.reported_at } : undefined
}

/** The reporter says how often it reports; a report older than five of its intervals means the sampler or the canary stopped. */
export function isStale(report: { reported_at: string; payload: { stale_after_seconds?: number } }, now: number): boolean {
  return now - Date.parse(report.reported_at) > (report.payload.stale_after_seconds ?? 300) * 1000
}

/** Everything the panel should call an open alert right now: resources over their thresholds, unsampleable hosts, failing canary steps, silent reporters. */
export function openAlerts(reports: Reports | undefined, now: number): Alert[] {
  const alerts: Alert[] = []
  const hosts = pick<HostsPayload>(reports, "hosts")
  const canary = pick<CanaryPayload>(reports, "canary")
  if (hosts && isStale(hosts, now)) alerts.push({ id: "hosts-stale", level: "critical", title: "Host capacity is not reported", body: "The production sampler stopped reporting.", since: hosts.reported_at })
  for (const [host, h] of Object.entries(hosts?.payload.hosts ?? {})) {
    if (h.unreachable) alerts.push({ id: `unreachable:${host}`, level: "critical", title: `${host} cannot be sampled`, body: "Its capacity is unknown." })
    for (const [name, r] of Object.entries(h.resources)) {
      if (r.level !== "normal") alerts.push({ id: `${host}:${name}`, level: r.level, title: `${host} ${name} ${r.level}`, body: `${name} is at ${r.percent}% of this host's capacity.` })
    }
  }
  if (canary && isStale(canary, now)) alerts.push({ id: "canary-stale", level: "critical", title: "The production canary is not reporting", body: "No canary result has arrived.", since: canary.reported_at })
  if (canary?.payload.red) {
    for (const [step, runs] of Object.entries(canary.payload.failing)) {
      const error = [canary.payload.last, canary.payload.account_last].flatMap((run) => run?.steps ?? []).find((s) => s.step === step && !s.ok)?.error
      alerts.push({ id: `canary:${step}`, level: "critical", title: `Canary step ${step} is failing`, body: `Failed ${runs} runs in a row${error ? `: ${error}` : ""}.`, since: canary.payload.red_since ?? undefined })
    }
  }
  return alerts.sort((a, b) => Number(b.level === "critical") - Number(a.level === "critical"))
}
