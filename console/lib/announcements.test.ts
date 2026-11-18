import { expect, test } from "bun:test"
import { checkResponse } from "@/lib/api-validate"

const banner = { id: "ann_1", title: "Beta", body: "", severity: "info", dismissible: true }
const notice = { id: "ann_2", title: "News", body: "", sent_at: "2026-11-18T00:00:00Z", read: false }

test("the announcements answer: no banner and no notices is a valid answer, a missing banner key is not", () => {
  expect(checkResponse("GET", "announcements", { banner: null, notices: [], unread: 0 })).toBeUndefined()
  expect(checkResponse("GET", "announcements", { banner, notices: [notice], unread: 1 })).toBeUndefined()
  expect(checkResponse("GET", "announcements", { notices: [], unread: 0 })).toBeDefined()
})
