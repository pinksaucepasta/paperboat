import { expect, test } from "bun:test"
import { openAlerts, type Reports } from "@/lib/host-health"

const now = Date.parse("2026-11-20T10:00:00Z")
const fresh = "2026-11-20T09:59:00Z"
const report = (source: "hosts" | "canary", payload: object, reported_at = fresh) => ({ source, payload, reported_at }) as Reports[number]

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
