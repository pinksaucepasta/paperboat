import { expect, test } from "bun:test"
import { formatMoney } from "@/lib/format"

test("money is shown in the currency's own unit", () => {
  expect(formatMoney(123456, "usd")).toBe("$1,234.56")
  expect(formatMoney(-500, "usd")).toBe("-$5.00")
  expect(formatMoney(1500, "jpy")).toBe("¥1,500")
  expect(formatMoney(0, "eur")).toBe("€0.00")
})
