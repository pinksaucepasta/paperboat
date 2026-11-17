/**
 * An Ed25519 key pair made in the browser for an edge's usage signing: the public key (base64, 32 bytes) goes to the
 * server at enrolment, the private seed (base64url, no padding) goes into the edge's usage key file. The seed is shown
 * once and never leaves the page. Returns undefined where the browser has no Ed25519 in WebCrypto.
 */
export async function generateUsageKeyPair(): Promise<{ publicKey: string; seed: string } | undefined> {
  try {
    const pair = (await crypto.subtle.generateKey("Ed25519", true, ["sign", "verify"])) as CryptoKeyPair
    const publicRaw = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey))
    const pkcs8 = new Uint8Array(await crypto.subtle.exportKey("pkcs8", pair.privateKey))
    const seed = pkcs8.slice(pkcs8.length - 32) // the PKCS#8 wrapper of an Ed25519 key ends with the 32-byte seed
    const b64 = (b: Uint8Array) => btoa(String.fromCharCode(...b))
    return { publicKey: b64(publicRaw), seed: b64(seed).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "") }
  } catch {
    return undefined
  }
}
