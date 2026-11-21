import { expect, test } from "bun:test"
import { openAlerts, type Reports } from "@/lib/host-health"

const now = Date.parse("2026-11-20T10:00:00Z")
const fresh = "2026-11-20T09:59:00Z"
const report = (source: string, payload: object, reported_at = fresh) => ({ source, payload, reported_at }) as Reports[number]

test("normal resources and a green canary raise nothing", () => {
  const reports = [
    report("hosts", { stale_after_seconds: 300, hosts: { db: { unreachable: false, resources: { memory: { percent: 40, level: "normal" } } } } }),
    report("canary", { stale_after_seconds: 300, red: false, red_since: null, failing: {}, last: null, account_last: null }),
  ]
  expect(openAlerts(reports, now)).toEqual([])
})

test("resource levels, unreachable hosts and failing canary steps are alerts, critical first", () => {
  const reports = [
    report("hosts", { stale_after_seconds: 300, hosts: {
      db: { unreachable: false, resources: { memory: { percent: 85, level: "warning" }, "disk /": { percent: 95, level: "critical" } } },
      edge: { unreachable: true, resources: {} } } }),
    report("canary", { stale_after_seconds: 300, red: true, red_since: "2026-11-20T09:50:00Z", failing: { exec_a_to_b: 3 },
      last: { started: "t", finished: "t", ok: false, steps: [{ step: "exec_a_to_b", ok: false, error: "machine unreachable" }] }, account_last: null }),
  ]
  const alerts = openAlerts(reports, now)
  expect(alerts.map((a) => a.id)).toEqual(["db:disk /", "unreachable:edge", "canary:exec_a_to_b", "db:memory"])
  expect(alerts[2].body).toBe("Failed 3 runs in a row: machine unreachable.")
})

test("a reporter that went quiet is an alert; a source that never reported is not", () => {
  const old = "2026-11-20T09:40:00Z"
  expect(openAlerts([report("hosts", { stale_after_seconds: 300, hosts: {} }, old)], now).map((a) => a.id)).toEqual(["hosts-stale"])
  expect(openAlerts([], now)).toEqual([])
  expect(openAlerts([report("canary", { red: true, failing: {} }, fresh)], now)).toEqual([])
})

test("an automatic rollback is an alert until a deploy stays", () => {
  const rolledBack = report("deploy", { rolled_back: true, kept_migrated: false, what: "server 2", reason: "no green canary run within 300 s of the switch", deploy_failed: false, green_after: true, rollback_ok: true, at: fresh })
  const alerts = openAlerts([rolledBack], now)
  expect(alerts.map((a) => a.id)).toEqual(["deploy:rolled-back"])
  expect(alerts[0].level).toBe("critical")
  expect(alerts[0].title).toBe("A deploy was rolled back automatically")
  expect(alerts[0].body).toContain("Fix the cause and deploy again")
  expect(openAlerts([report("deploy", { rolled_back: false, kept_migrated: false, what: "server 2", at: fresh })], now)).toEqual([])
})

test("a deploy kept on the migrated schema is a roll-forward alert", () => {
  const kept = report("deploy", { rolled_back: false, kept_migrated: true, what: "server 2", at: fresh })
  const alerts = openAlerts([kept], now)
  expect(alerts.map((a) => a.id)).toEqual(["deploy:kept-migrated"])
  expect(alerts[0].level).toBe("critical")
  expect(alerts[0].body).toContain("Deploy a fix forward")
})

test("a failing edge certificate renewal is a critical alert; a quiet healthy edge is a warning", () => {
  const failing = report("edge-certificate:203.0.113.7", { ok: false, error: "issue failed", stale_after_seconds: 86400 })
  const healthy = report("edge-certificate:203.0.113.8", { ok: true, error: "", stale_after_seconds: 86400 })
  const alerts = openAlerts([failing, healthy], now)
  expect(alerts.map((a) => a.id)).toEqual(["edge-certificate:203.0.113.7"])
  expect(alerts[0].level).toBe("critical")
  expect(alerts[0].title).toContain("Certificate renewal is failing on 203.0.113.7")
  expect(alerts[0].body).toContain("renew.log")
  const old = "2026-11-19T09:00:00Z"
  const staleHealthy = openAlerts([report("edge-certificate:203.0.113.8", { ok: true, error: "", stale_after_seconds: 86400 }, old)], now)
  expect(staleHealthy.map((a) => a.id)).toEqual(["edge-certificate-stale:203.0.113.8"])
  expect(staleHealthy[0].level).toBe("warning")
})
