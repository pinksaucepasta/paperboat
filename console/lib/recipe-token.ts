"use client"

/*
 * A recipe token, made in the browser exactly as shared/recipe/token.go makes it: `pbr_<id>_<secret>`,
 * id 10 and secret 52 lowercase base32 characters (32 random bytes). The server gets only `auth`, an
 * HKDF-SHA256 of the secret, so it can admit a machine but never holds the secret. `publicKey` is the X25519 public key
 * of another HKDF of the secret (Token.PublicKey): vault keys are wrapped to it, so the server never reads a vault.
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

export type NewToken = { id: string; token: string; auth: string; publicKey: string }

/** The public id of a listed recipe's token (`pbr_<id>_…`). */
export const tokenId = (masked: string) => tokenPattern.exec(masked)?.[1]

const hkdf = async (key: CryptoKey, info: string) =>
  new Uint8Array(
    await crypto.subtle.deriveBits({ name: "HKDF", hash: "SHA-256", salt: new Uint8Array(0), info: new TextEncoder().encode(info) }, key, 256),
  )

// An X25519 private key in PKCS#8: this prefix, then its 32 bytes.
const pkcs8Prefix = [0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x6e, 0x04, 0x22, 0x04, 0x20]

/** The X25519 public key (standard base64) of a private key, through WebCrypto's JWK export. */
async function publicOf(priv: Uint8Array): Promise<string> {
  const key = await crypto.subtle.importKey("pkcs8", new Uint8Array([...pkcs8Prefix, ...priv]), { name: "X25519" }, true, ["deriveBits"])
  const x = (await crypto.subtle.exportKey("jwk", key)).x ?? ""
  return base64(Uint8Array.from(atob(x.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0)))
}

/** The token for `secret`, with the `auth` the server stores a hash of and the `publicKey` vault keys are wrapped to. */
export async function tokenFrom(id: string, secret: Uint8Array<ArrayBuffer>): Promise<NewToken> {
  const key = await crypto.subtle.importKey("raw", secret, "HKDF", false, ["deriveBits"])
  const auth = await hkdf(key, "paperboat recipe auth")
  return { id, token: `pbr_${id}_${base32(secret)}`, auth: base64(auth), publicKey: await publicOf(await hkdf(key, "paperboat recipe key")) }
}

/** A token with a fresh secret; `id` keeps a recipe's token id (rotate), none makes a new one. */
export const newToken = (id?: string) =>
  tokenFrom(id ?? base32(crypto.getRandomValues(new Uint8Array(7))).slice(0, 10), crypto.getRandomValues(new Uint8Array(32)))
