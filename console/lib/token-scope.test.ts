import { expect, test } from "bun:test"
import { actionNames, daysUntil, machineOptions, tokenScope } from "@/lib/token-scope"

test("a scope names all machines as machine:* and keeps actions in the console's order", () => {
  expect(tokenScope("all", ["files", "terminal"])).toEqual([{ dst: ["machine:*"], allow: ["terminal", "files"] }])
  expect(tokenScope(["tag:prod", "machine:hp"], ["exec"])).toEqual([{ dst: ["tag:prod", "machine:hp"], allow: ["exec"] }])
  expect(actionNames(["files", "terminal"])).toBe("Terminal, Files")
})

test("a custom end date is the whole days from now to that date, never below one", () => {
  const now = new Date(2026, 10, 9, 12).getTime()
  expect(daysUntil("2026-11-10", now)).toBe(1)
  expect(daysUntil("2027-02-08", now)).toBe(91)
  expect(daysUntil("2026-11-01", now)).toBe(1)
})

test("machines that share an alias are two options, each with the policy's disambiguated target and its own owner's status", () => {
  const machines = [
    { alias: "web", online: true, owner_account: "a1" },
    { alias: "web", online: false, owner_account: "a2" },
    { alias: "hp", online: true, owner_account: "a1" },
  ]
  const emails = new Map([["a1", "Ann@x.test"], ["a2", "bob@x.test"]])
  expect(machineOptions(["ann@x.test/web", "bob@x.test/web", "hp"], machines, emails)).toEqual([
    { target: "machine:ann@x.test/web", alias: "web", owner: "ann@x.test", online: true },
    { target: "machine:bob@x.test/web", alias: "web", owner: "bob@x.test", online: false },
    { target: "machine:hp", alias: "hp", owner: undefined, online: true },
  ])
})
