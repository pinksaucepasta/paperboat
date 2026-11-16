"use client"

import { api, ApiError, idempotencyKey, type Data } from "@/lib/api"
import { vaultCrypto, vaultUnlocked } from "@/lib/envvault/crypto"

/*
 * Named ENV vaults for recipes (PLAN F26), with the CLI's own code in the browser's vault engine
 * (cli/cmd/environmentvault-wasm): `named-vault-wraps` makes a new vault's key wrapped for this account (and
 * team), `named-vault-recipe-wrap` opens a vault's key with the unlocked ENV keys and wraps it to a recipe's
 * public key. The keys stay in the worker; the server stores only wraps.
 */

type Vault = Data<"/v1/workspaces/{ws}/env-vaults/{name}">["vault"]
/** A vault key wrapped to a recipe (`vault_wraps` of a recipe create or update). */
export type VaultWrap = { vault: string; wrapped: string }

/** The recipe reads a vault, and the ENV vault is locked in this browser, so its key cannot be wrapped yet. */
export class VaultLockedError extends Error {
  constructor(readonly vault: string) {
    super(`This recipe reads the vault ${vault}. Unlock ENV to give the recipe its key.`)
  }
}

const vaultPath = (workspace: string, name: string) => `workspaces/${encodeURIComponent(workspace)}/env-vaults/${encodeURIComponent(name)}`

/** `vault_key_missing`: the server needs a key wrapped for this vault; its name. */
export const missingVault = (error: unknown) =>
  error instanceof ApiError && error.code === "vault_key_missing" ? (error.details as { vault?: string } | undefined)?.vault : undefined

/** `unknown_vault`: the recipe names a vault the workspace does not have; its name. */
export const unknownVault = (error: unknown) =>
  error instanceof ApiError && error.code === "unknown_vault" ? (error.details as { name?: string } | undefined)?.name : undefined

/** One vault's key, wrapped to the recipe whose token has this id and public key. */
export async function wrapVault(workspace: string, name: string, token: { id: string; publicKey: string }): Promise<VaultWrap> {
  if (!vaultUnlocked()) throw new VaultLockedError(name)
  const { vault } = await api<Data<"/v1/workspaces/{ws}/env-vaults/{name}">>(vaultPath(workspace, name))
  return vaultCrypto<VaultWrap>({
    action: "named-vault-recipe-wrap",
    name,
    vault_wraps: vault.wraps,
    recipe_public_key: token.publicKey,
    token_id: token.id,
  })
}

/** A recipe may read at most this many vaults per save here; the server's own limit is higher. */
const maxVaults = 16

/**
 * Sends a recipe write; when the server says the recipe newly reads a vault, wraps that vault's key to the
 * recipe (unlocked ENV needed) and sends again, so the file is read once, by the server, as in the CLI.
 */
export const withVaultKeys = <T>(
  workspace: string,
  token: { id: string; publicKey: string },
  send: (wraps: VaultWrap[]) => Promise<T>,
  have: VaultWrap[] = [],
) => withWraps(send, (name) => wrapVault(workspace, name, token), have)

/** The retry loop of withVaultKeys, with the wrapping of one vault passed in. */
export async function withWraps<T>(send: (wraps: VaultWrap[]) => Promise<T>, wrap: (vault: string) => Promise<VaultWrap>, have: VaultWrap[] = []): Promise<T> {
  const wraps = [...have]
  for (;;) {
    try {
      return await send(wraps)
    } catch (error) {
      const name = missingVault(error)
      if (!name || wraps.some((w) => w.vault === name) || wraps.length >= maxVaults) throw error
      wraps.push(await wrap(name))
    }
  }
}

/** Makes a vault named `name` in the workspace (a team's vault is wrapped for the team too); needs unlocked ENV. */
export async function createNamedVault(workspace: string, name: string, teamId?: string): Promise<void> {
  if (!vaultUnlocked()) throw new VaultLockedError(name)
  const { wraps } = await vaultCrypto<{ wraps: Vault["wraps"] }>({ action: "named-vault-wraps", name, team_id: teamId })
  await api(`workspaces/${encodeURIComponent(workspace)}/env-vaults`, {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": idempotencyKey() },
    body: JSON.stringify({ name, wraps }),
  })
}
