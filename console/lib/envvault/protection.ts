"use client"

import { api, ApiError, type Me } from "@/lib/api"
import { lockVault, vaultCrypto } from "@/lib/envvault/crypto"
import { currentVault, readVault, vaultIssuer, type Vault } from "@/lib/envvault/vault"

/*
 * Vault setup, password change and the recovery code in the browser: `pb env vault init`,
 * `password`, `recovery` and `recover` with the CLI's own envelopes (the vault WASM). The password
 * and the codes stay in the worker; the server only receives the sealed vault. A code is shown
 * before anything is published, so a closed tab never leaves a vault protected by a code nobody saw.
 */

const json = { "content-type": "application/json" }
const path = "environment-vault?workspace=personal"

/** A new recovery code, to show before it is used. */
export const newRecoveryCode = () => vaultCrypto<{ code: string }>({ action: "recovery-code" }).then((r) => r.code)

/** Whether the unlocked vault can also be opened with a recovery code. */
export async function recoveryEnabled(): Promise<boolean> {
  const vault = await currentVault()
  return (await vaultCrypto<{ recovery_enabled: boolean }>({ action: "status", vault })).recovery_enabled
}

/**
 * Publishes a sealed vault and opens it. A lost response may still have committed it; any other
 * failure drops the browser's keys, so nothing half-changed stays open.
 */
async function publish(staged: { envelope: string }): Promise<void> {
  let state: Vault
  try {
    state = await api<Vault>(path, {
      method: "PUT",
      headers: json,
      body: JSON.stringify({ envelope: staged.envelope }),
    })
  } catch (error) {
    const committed = await readVault().catch(() => null)
    if (committed?.envelope !== staged.envelope) {
      lockVault()
      if (error instanceof ApiError && (error.status === 409 || error.status === 412)) {
        throw new Error("The vault changed on another device. Reload and try again.")
      }
      throw error
    }
    state = committed
  }
  await vaultCrypto({ action: "commit", vault: state })
}

/** Creates the vault with a password and a recovery code the caller has shown. */
export async function setUpVault(password: string, code: string): Promise<void> {
  const [issuer, me] = await Promise.all([vaultIssuer(), api<Me>("me")])
  const staged = await vaultCrypto<{ envelope: string }>({
    action: "initialize",
    vault: { issuer, account_id: me.id },
    password,
    code,
  })
  await publish(staged)
}

/** Replaces the password of the unlocked vault. */
export async function changePassword(password: string): Promise<void> {
  const vault = await currentVault()
  await publish(await vaultCrypto<{ envelope: string }>({ action: "password", vault, password }))
}

/** Enables or replaces the recovery code with one the caller has shown; an empty code turns recovery off. */
export async function replaceRecoveryCode(code: string): Promise<void> {
  const vault = await currentVault()
  await publish(await vaultCrypto<{ envelope: string }>({ action: "recovery", vault, code }))
}

/**
 * After `unlockVault(code, "recovery")`: a new password and a new recovery code in one vault version,
 * like `pb env vault recover`. The old code stops working.
 */
export async function recoverVault(password: string, code: string): Promise<void> {
  const vault = await currentVault()
  await publish(
    await vaultCrypto<{ envelope: string }>({ action: "password", vault, password, enable_recovery: true, code }),
  )
}
