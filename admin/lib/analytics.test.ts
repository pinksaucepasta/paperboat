import { expect, test } from "bun:test"
import { dropoff } from "@/lib/analytics"

test("the funnel drop-off is the step that loses the largest share", () => {
  expect(dropoff([{ count: 100 }, { count: 60 }, { count: 50 }])).toBe(1)
  expect(dropoff([{ count: 100 }, { count: 90 }, { count: 20 }])).toBe(2)
})

test("a funnel that loses nothing, or is empty, has no drop-off", () => {
  expect(dropoff([{ count: 5 }, { count: 5 }])).toBe(-1)
  expect(dropoff([])).toBe(-1)
  expect(dropoff([{ count: 0 }, { count: 0 }])).toBe(-1)
})
