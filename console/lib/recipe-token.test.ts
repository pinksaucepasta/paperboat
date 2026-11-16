import { expect, test } from "bun:test"
import { newToken, tokenFrom, tokenId } from "@/lib/recipe-token"

// The expected values come from an independent HKDF-SHA256 / base32 (shared/recipe/token.go's definition).
test("a token derives its auth the way shared/recipe does", async () => {
  const made = await tokenFrom("abcdefghij", Uint8Array.from({ length: 32 }, (_, i) => i))
  expect(made.token).toBe("pbr_abcdefghij_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq")
  expect(made.auth).toBe("xJtv9QXERzWmQ5fuFqokaHxmhZ5liHIpALkHdzqJ9JQ=")
  // HKDF-SHA256 "paperboat recipe key", then X25519 (Python cryptography), for the same secret.
  expect(made.publicKey).toBe("wkZEQafgKPkDFV+zmfJURuecdF9XYLcEKv9BGKyHe1c=")
})

test("a new token has the shared shape and rotate keeps the id", async () => {
  const first = await newToken()
  expect(first.token).toMatch(/^pbr_[a-z2-7]{10}_[a-z2-7]{52}$/)
  expect(first.token.length).toBe(67)
  const next = await newToken(first.id)
  expect(next.id).toBe(first.id)
  expect(next.token).not.toBe(first.token)
  expect(tokenId(`pbr_${first.id}_…`)).toBe(first.id)
  expect(tokenId("pbr_x")).toBeUndefined()
})
