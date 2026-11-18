import { expect, test } from "bun:test"
import { readFileSync } from "node:fs"
import { validVariableName } from "@/lib/envvault/names"

// The list shared/envname's Go test reads: one rule for the server, the CLI, the engine and this console.
const cases = JSON.parse(
  readFileSync(new URL("../../../shared/envname/testdata/names.json", import.meta.url), "utf8"),
) as {
  name: string
  valid: boolean
}[]

test("the console's variable-name rule agrees with the shared list", () => {
  expect(cases.length).toBeGreaterThan(20)
  for (const { name, valid } of cases) expect([name.length, validVariableName(name)]).toEqual([name.length, valid])
})
