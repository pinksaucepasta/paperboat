import { expect, test } from "bun:test"
import { formatMoney, formatUnseen } from "@/lib/format"

test("money is shown in the currency's own unit", () => {
  expect(formatMoney(123456, "usd")).toBe("$1,234.56")
  expect(formatMoney(-500, "usd")).toBe("-$5.00")
  expect(formatMoney(1500, "jpy")).toBe("¥1,500")
  expect(formatMoney(0, "eur")).toBe("€0.00")
})

test("unseen time is an exact count: minutes, hours under a day, whole days after", () => {
  const now = Date.parse("2026-11-18T09:00:00Z")
  const ago = (ms: number) => new Date(now - ms).toISOString()
  expect(formatUnseen(ago(30_000), now)).toBe("0 min")
  expect(formatUnseen(ago(59 * 60_000), now)).toBe("59 min")
  expect(formatUnseen(ago(3 * 3_600_000 + 40 * 60_000), now)).toBe("3 h")
  expect(formatUnseen(ago(23 * 3_600_000 + 59 * 60_000), now)).toBe("23 h")
  expect(formatUnseen(ago(24 * 3_600_000), now)).toBe("1 day")
  expect(formatUnseen(ago(12 * 86_400_000 + 5 * 3_600_000), now)).toBe("12 days")
  expect(formatUnseen(ago(5_000), 0)).toBe("")
})
