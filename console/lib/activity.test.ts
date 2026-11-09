import { expect, test } from "bun:test"
import { describe as sentence, type ActivityEvent } from "@/lib/activity"

const event = (action: string) => ({ action, actor: { kind: "system", id: "x" } }) as ActivityEvent

test("events seen on the server read as sentences", () => {
  expect(sentence(event("user_machine.final_deleted"))).toBe("finished removing")
  expect(sentence(event("machine.access_ended"))).toBe("closed access to")
})

test("an unknown event still reads as a sentence, without the server's code", () => {
  expect(sentence(event("user_machine.foo_bar"))).toBe("changed")
})
