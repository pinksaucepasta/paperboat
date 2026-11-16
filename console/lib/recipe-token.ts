"use client"

/*
 * A recipe token, made in the browser exactly as shared/recipe/token.go makes it: `pbr_<id>_<secret>`,
 * id 10 and secret 52 lowercase base32 characters (32 random bytes). The server gets only `auth`, an
 * HKDF-SHA256 of the secret, so it can admit a machine but never holds the secret.
 */

const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
const tokenPattern = /^pbr_([a-z2-7]{10})_…$/

function base32(bytes: Uint8Array): string {
  let out = ""
  let bits = 0
  let value = 0
  for (const byte of bytes) {
    value = (value << 8) | byte
    bits += 8
    for (; bits >= 5; bits -= 5) out += alphabet[(value >>> (bits - 5)) & 31]
  }
  return bits > 0 ? out + alphabet[(value << (5 - bits)) & 31] : out
}

const base64 = (bytes: Uint8Array) => btoa(String.fromCharCode(...bytes))

export type NewToken = { id: string; token: string; auth: string }

/** The public id of a listed recipe's token (`pbr_<id>_…`). */
export const tokenId = (masked: string) => tokenPattern.exec(masked)?.[1]

/** The token for `secret`, with the `auth` the server stores a hash of. */
export async function tokenFrom(id: string, secret: Uint8Array<ArrayBuffer>): Promise<NewToken> {
  const key = await crypto.subtle.importKey("raw", secret, "HKDF", false, ["deriveBits"])
  const info = new TextEncoder().encode("paperboat recipe auth")
  const auth = await crypto.subtle.deriveBits({ name: "HKDF", hash: "SHA-256", salt: new Uint8Array(0), info }, key, 256)
  return { id, token: `pbr_${id}_${base32(secret)}`, auth: base64(new Uint8Array(auth)) }
}

/** A token with a fresh secret; `id` keeps a recipe's token id (rotate), none makes a new one. */
export const newToken = (id?: string) =>
  tokenFrom(id ?? base32(crypto.getRandomValues(new Uint8Array(7))).slice(0, 10), crypto.getRandomValues(new Uint8Array(32)))
