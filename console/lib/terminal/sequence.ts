/**
 * Terminal output sequences are unsigned 64-bit decimals from the host. They are compared as
 * strings so they never lose precision in a JavaScript number.
 */

export function normalizeSequence(value: string): string {
  if (!/^\d{1,20}$/.test(value)) throw new Error("The terminal returned an invalid sequence number.")
  return value.replace(/^0+(?=\d)/, "")
}

export function compareSequence(left: string, right: string): number {
  const a = normalizeSequence(left)
  const b = normalizeSequence(right)
  if (a.length !== b.length) return a.length < b.length ? -1 : 1
  return a === b ? 0 : a < b ? -1 : 1
}

function subtractSequence(left: string, right: string): string {
  const a = normalizeSequence(left)
  const b = normalizeSequence(right)
  if (compareSequence(a, b) < 0) throw new Error("Terminal output sequences moved backwards.")
  let borrow = 0
  let result = ""
  for (let offset = 0; offset < a.length; offset++) {
    const aIndex = a.length - 1 - offset
    const bIndex = b.length - 1 - offset
    let digit = Number(a[aIndex]) - borrow - (bIndex >= 0 ? Number(b[bIndex]) : 0)
    borrow = digit < 0 ? 1 : 0
    if (digit < 0) digit += 10
    result = String(digit) + result
  }
  return result.replace(/^0+(?=\d)/, "")
}

export function sequenceOffset(later: string, earlier: string): number {
  const difference = subtractSequence(later, earlier)
  return difference.length > 7 ? Number.MAX_SAFE_INTEGER : Number(difference)
}
