import { expect, test } from "bun:test"
import { statusUrl } from "./status-url"

test("status URL: override wins, else status.<site domain> for https, else none", () => {
  expect(statusUrl("https://www.example.test", "https://s.example.test")).toBe("https://s.example.test")
  expect(statusUrl("https://www.example.test", undefined)).toBe("https://status.example.test")
  expect(statusUrl("http://localhost:3000", undefined)).toBeUndefined()
  expect(statusUrl(undefined, undefined)).toBeUndefined()
})
