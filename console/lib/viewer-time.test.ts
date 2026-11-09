import { describe, expect, test } from "bun:test"
import { formatDate, viewerFrom } from "@/lib/format"

describe("dates in the viewer's zone and locale", () => {
  test("a cookie zone and an Accept-Language make a viewer; anything unusable makes none", () => {
    expect(viewerFrom("Asia/Kolkata", "en-GB,en;q=0.9")).toEqual({ zone: "Asia/Kolkata", locale: "en-GB" })
    expect(viewerFrom("Not/AZone", "en-GB")).toBeUndefined()
    expect(viewerFrom(undefined, "en-GB")).toBeUndefined()
    expect(viewerFrom("UTC", null)).toBeUndefined()
  })

  test("the same instant reads as the viewer's local day, not UTC's", () => {
    // 20:00 UTC on Nov 17 is already Nov 18 in Kolkata (UTC+5:30).
    const iso = "2026-11-17T20:00:00Z"
    expect(formatDate(iso, "day", { zone: "Asia/Kolkata", locale: "en-GB" })).toBe("18 Nov")
    expect(formatDate(iso, "day", { zone: "UTC", locale: "en-US" })).toBe("Nov 17")
  })
})
