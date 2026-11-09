import { expect, test } from "bun:test"
import { savedBy, type Policy } from "@/lib/policy"

const base = { workspace: "w", version: 1, policy: "{}", digest: "", updated_at: "", message: "", can_edit: "all", names: {} }

test("a policy the server saved itself (no updated_by) has no author instead of crashing the editor", () => {
  expect(savedBy(base as unknown as Policy)).toBe("")
  expect(savedBy({ ...base, updated_by: "user:a@b.c" } as unknown as Policy)).toBe(" by a@b.c")
  expect(savedBy({ ...base, updated_by: "user:a@b.c", updated_with_api_token: "ci" } as unknown as Policy)).toBe(
    " by a@b.c with API token ci",
  )
})
