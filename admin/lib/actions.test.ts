import { afterEach, expect, test } from "bun:test"
import { installCommand } from "@/components/infra/selfhost"
import { enrollNodeCommand } from "@/components/infra/server-nodes"
import { dateVersion } from "@/components/releases"
import { ApiError, operatorAction, referencePattern } from "@/lib/api"
import { sectionsFor } from "@/lib/sections"

const realFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = realFetch
})

test("an operator action sends the target, reason and idempotency key as headers and the body as JSON", async () => {
  let seen: { url: string; init: RequestInit } | undefined
  globalThis.fetch = (async (url: string, init: RequestInit) => {
    seen = { url, init }
    return new Response(null, { status: 204 })
  }) as unknown as typeof fetch
  const out = await operatorAction("POST", "/v1/admin/nodes/n1/drain", { target: "n1", reason: "TICKET-1", key: "key-12345678" }, { revision: 4 })
  expect(out).toBeUndefined()
  const headers = seen!.init.headers as Record<string, string>
  expect(seen!.url).toBe("/api/v1/admin/nodes/n1/drain")
  expect([headers["operator-target"], headers["operator-reason"], headers["idempotency-key"]]).toEqual(["n1", "TICKET-1", "key-12345678"])
  expect(seen!.init.body).toBe('{"revision":4}')
})

test("a refused operator action fails with the server's message and support reference", async () => {
  globalThis.fetch = (async () =>
    new Response(JSON.stringify({ error: { code: "operator_conflict", message: "Stale revision.", support_reference: "support_abc" } }), { status: 409 })) as unknown as typeof fetch
  const error = await operatorAction("PUT", "/v1/admin/config/billing", { target: "billing", reason: "T-1", key: "key-12345678" }, {}).catch((e: unknown) => e as ApiError)
  expect(error).toBeInstanceOf(ApiError)
  expect([(error as ApiError).status, (error as ApiError).code, (error as ApiError).reference]).toEqual([409, "operator_conflict", "support_abc"])
})

test("a lost connection says the action may have been applied", async () => {
  globalThis.fetch = (async () => {
    throw new TypeError("network down")
  }) as unknown as typeof fetch
  const error = (await operatorAction("POST", "/v1/admin/x/y", { target: "x", reason: "T-1", key: "key-12345678" }).catch((e: unknown) => e)) as ApiError
  expect(error.status).toBe(0)
  expect(error.message).toContain("may have been applied")
})

test("the audit reference accepts the server's charset and nothing else", () => {
  for (const ok of ["TICKET-42", "a.b_c:d-e", "x".repeat(128)]) expect(referencePattern.test(ok)).toBe(true)
  for (const bad of ["", "has space", "emoji🙂", "x".repeat(129), "a/b", "a\nb"]) expect(referencePattern.test(bad)).toBe(false)
})

test("the pbh install command carries the kind, host, region and a quote-safe label", () => {
  expect(installCommand("relay", "relay-1.example.test", "hel", "")).toBe("sudo pbh install --relay --endpoint-host relay-1.example.test --region hel")
  expect(installCommand("both", "h.example.test", "sin", "Bob's box")).toBe("sudo pbh install --both --endpoint-host h.example.test --region sin --name 'Bobs box'")
})

test("the server-node enrol command names region, instance and host", () => {
  expect(enrollNodeCommand("hel", "slot-02", "s.example.test")).toBe("paperboat-server enroll-node -region hel -instance slot-02 -host s.example.test -config-out /etc/paperboat/node.json")
})

test("release versions are real calendar dates with a build number", () => {
  for (const ok of ["2026.11.18.1", "2026.1.2.30", "2028.2.29.1"]) expect(dateVersion(ok)).toBe(true)
  for (const bad of ["", "1.2.3", "2026.11.18", "v2026.11.18.1", "2026.11.18.1.2", "2026.11.18.x", "2026.13.1.1", "2026.2.30.1", "2027.2.29.1", "0000.1.1.1"]) expect(dateVersion(bad)).toBe(false)
})

test("only administrators see the release pages", () => {
  const has = (role: "admin" | "support" | "billing" | "operations") => sectionsFor(role).some((s) => s.href === "/releases")
  expect([has("admin"), has("support"), has("billing"), has("operations")]).toEqual([true, false, false, false])
})

test("only administrators see the operators page", () => {
  const has = (role: "admin" | "support" | "billing" | "operations") => sectionsFor(role).some((s) => s.href === "/operators")
  expect([has("admin"), has("support"), has("billing"), has("operations")]).toEqual([true, false, false, false])
})
