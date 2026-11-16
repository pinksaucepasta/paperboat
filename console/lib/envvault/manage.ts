"use client"

import { api, ApiError, type Me } from "@/lib/api"
import { lockVault, vaultCrypto } from "@/lib/envvault/crypto"
import { publish } from "@/lib/envvault/protection"
import {
  currentVault,
  operationId,
  readRecords,
  readVault,
  type EnvRecord,
  type EnvScope,
  type Vault,
} from "@/lib/envvault/vault"

/*
 * Team vaults and key changes in the browser: what `pb env team …`, `pb env rotate` and `pb env vault reset`
 * do, with the same WASM engine and the same server calls. The keys stay in the worker; each call here only
 * feeds it server state and sends back the sealed result. Every flow needs the vault unlocked, except the
 * reset of a vault whose password is lost.
 */

export type TeamMember = {
  account_id: string
  membership_generation: number
  role: string
  active: boolean
  grant_epoch: number
  env_permission: string
}
export type VaultTeam = {
  team_id: string
  owner_account: string
  generation: number
  key_epoch: number
  members: TeamMember[]
  rotation_required: boolean
  entitled: boolean
  record_sequence: number
}
type Sharing = { account_id: string; vault_generation: number; sharing_public: string }
type Grant = {
  team_id: string
  membership_generation: number
  team_epoch: number
  document_id: string
  envelope: string
  sender_writer_public: string
  acknowledged: boolean
}
type PersonalScope = {
  workspace_id: string
  machine_id: string
  document_id: string
  [key: string]: unknown
}

const json = { "content-type": "application/json" }
const post = <T>(path: string, body: unknown) =>
  api<T>(path, { method: "POST", headers: json, body: JSON.stringify(body) })
const inTeam = (team: string) => `workspace=${encodeURIComponent(team)}`
const teamPath = (team: string) => `environment/teams/${encodeURIComponent(team)}?${inTeam(team)}`

/** The team's encrypted state, or null before its vault was created. */
export async function readTeamVault(team: string, signal?: AbortSignal): Promise<VaultTeam | null> {
  try {
    return await api<VaultTeam>(teamPath(team), { signal })
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) return null
    throw error
  }
}

/**
 * After a server call that replaced the vault (create, rotate, reset): the new vault becomes the open
 * one. A lost response may still have committed the change; any other failure locks the vault so nothing
 * half-changed stays open.
 */
async function commitReplacement(staged: string, send: () => Promise<unknown>): Promise<void> {
  try {
    await send()
  } catch (error) {
    const committed = await readVault().catch(() => null)
    if (committed?.envelope !== staged) {
      lockVault()
      throw error
    }
  }
  const state = await readVault()
  if (state?.envelope !== staged) {
    lockVault()
    throw new Error("The vault changed on another device. Unlock it again and retry.")
  }
  await vaultCrypto({ action: "commit", vault: state })
}

const ownMembership = (team: VaultTeam, account: string) => {
  const own = team.members.find((member) => member.account_id === account && member.active)
  if (!own) throw new Error("Only an accepted member of the team can do this.")
  return own.membership_generation
}

/** Creates the team's encrypted vault; the team's owner does this once. */
export async function createTeamVault(team: string): Promise<void> {
  const [vault, me, state] = await Promise.all([currentVault(), api<Me>("me"), readTeamVault(team)])
  if (!state) throw new Error("This team can't hold variables yet.")
  const sealed = await vaultCrypto<{
    operation_id: string
    scope_envelope: string
    grant_envelope: string
    vault_envelope: string
    expected_team_generation: number
  }>({
    action: "team-create",
    vault,
    team: state,
    owner: team,
    operation_id: operationId(),
    membership_generation: ownMembership(state, me.id),
  })
  await commitReplacement(sealed.vault_envelope, () =>
    post(`environment/teams?${inTeam(team)}`, { ...sealed, team_id: team }),
  )
}

/** Gives a member the team's key, sealed to their vault. Owners and admins grant. */
export async function grantTeamKey(team: string, account: string): Promise<void> {
  const [vault, state, sharing] = await Promise.all([
    currentVault(),
    readTeamVault(team),
    api<Sharing>(`environment/users/${encodeURIComponent(account)}/sharing?workspace=personal`),
  ])
  if (!state) throw new Error("This team has no vault yet.")
  const sealed = await vaultCrypto<Record<string, unknown>>({
    action: "team-grant",
    vault,
    team: state,
    owner: team,
    operation_id: operationId(),
    recipient: account,
    sharing: [sharing],
  })
  await post(`environment/teams/${encodeURIComponent(team)}/members?${inTeam(team)}`, sealed)
}

/**
 * Takes the keys other members granted this account: each is added to the vault, then acknowledged.
 * Returns how many were accepted.
 */
export async function acceptTeamKeys(): Promise<number> {
  let accepted = 0
  const { grants } = await api<{ grants: Grant[] }>("environment/grants?workspace=personal")
  for (const grant of grants.filter((entry) => !entry.acknowledged)) {
    const [vault, state] = await Promise.all([currentVault(), readTeamVault(grant.team_id)])
    if (!state) continue
    const result = await vaultCrypto<{ envelope?: string; document_id: string; already_current?: boolean }>({
      action: "team-accept",
      vault,
      team: state,
      owner: grant.team_id,
      grant: {
        team_id: grant.team_id,
        membership_generation: grant.membership_generation,
        team_epoch: grant.team_epoch,
        document_id: grant.document_id,
        envelope: grant.envelope,
        sender_writer_public: grant.sender_writer_public,
      },
    })
    if (result.envelope) await publish({ envelope: result.envelope })
    await post(`environment/grants/${encodeURIComponent(grant.document_id)}/ack?workspace=personal`, {
      vault_document_id: result.document_id,
    })
    accepted++
  }
  return accepted
}

/**
 * Replaces the team's key, re-encrypting its variables and giving the remaining members the new key.
 * `remove` members lose access; `lostKey` (owner only) starts over and discards the variables.
 */
export async function rotateTeamVault(team: string, options: { remove?: string[]; lostKey?: boolean } = {}) {
  const remove = options.remove ?? []
  const [vault, state] = await Promise.all([currentVault(), readTeamVault(team)])
  if (!state) throw new Error("This team has no vault yet.")
  const removed = new Set(remove)
  const sharing = await Promise.all(
    state.members
      .filter((member) => member.active && !removed.has(member.account_id) && ["read", "write"].includes(member.env_permission))
      .map((member) =>
        api<Sharing>(`environment/users/${encodeURIComponent(member.account_id)}/sharing?workspace=personal`),
      ),
  )
  const scope: EnvScope = { kind: "team", owner: team, machine: "", workspace: team }
  const frozen: { records: EnvRecord[]; sequence: number } = options.lostKey
    ? { records: [], sequence: state.record_sequence }
    : await readRecords(scope)
  const sealed = await vaultCrypto<{ vault_envelope: string }>({
    action: "team-rotate",
    vault,
    team: state,
    owner: team,
    operation_id: operationId(),
    remove_account_ids: remove,
    confirm_total_loss: Boolean(options.lostKey),
    sharing,
    records: frozen.records,
    record_sequence: frozen.sequence,
  })
  await commitReplacement(sealed.vault_envelope, () =>
    post(`environment/teams/${encodeURIComponent(team)}/rotate?${inTeam(team)}`, sealed),
  )
}

type Inventory = { rotation_required: boolean; key_epoch: number; scopes: PersonalScope[] }
const readInventory = () => api<Inventory>("environment/scopes?workspace=personal")

/** Replaces the account's personal key and re-encrypts every variable with it; credentials stay. */
export async function rotatePersonalKey(): Promise<void> {
  const [vault, me, inventory] = await Promise.all([currentVault(), api<Me>("me"), readInventory()])
  const operation = operationId()
  const staged = await vaultCrypto<{ envelope: string; document_id: string }>({
    action: "personal-rotate",
    vault,
    inventory: inventory.scopes,
    key_epoch: inventory.key_epoch,
  })
  try {
    const documents: { workspace_id: string; machine_id: string; document_id: string }[] = []
    for (const head of inventory.scopes) {
      const scope: EnvScope = { kind: "personal", owner: me.id, machine: head.machine_id, workspace: head.workspace_id }
      const { records, sequence } = await readRecords(scope)
      const next = await vaultCrypto<{ envelope: string; document_id: string; records: unknown }>({
        action: "personal-rotate-scope",
        vault,
        scope: head,
        records,
        record_sequence: sequence,
      })
      await api(`environment/rotate-personal/${encodeURIComponent(operation)}/scopes?workspace=personal`, {
        method: "PUT",
        headers: json,
        body: JSON.stringify({
          workspace_id: head.workspace_id,
          machine_id: head.machine_id,
          expected_vault_document_id: vault.document_id,
          envelope: next.envelope,
          records: next.records,
        }),
      })
      documents.push({ workspace_id: head.workspace_id, machine_id: head.machine_id, document_id: next.document_id })
    }
    await commitReplacement(staged.envelope, () =>
      post("environment/rotate-personal?workspace=personal", {
        operation_id: operation,
        expected_vault_document_id: vault.document_id,
        vault_envelope: staged.envelope,
        scope_documents: documents,
      }),
    )
  } catch (error) {
    // Drop the half-staged rotation on the server; the vault stays as it was.
    await api(`environment/rotate-personal/${encodeURIComponent(operation)}?workspace=personal`, { method: "DELETE" }).catch(
      () => undefined,
    )
    lockVault()
    throw error
  }
}

/**
 * Starts the vault over with a new password: every personal variable and the team keys are gone. For a
 * vault whose password and recovery code are lost; no unlock needed. The new vault opens afterwards.
 */
export async function resetVault(password: string): Promise<void> {
  const vault = await readVault()
  if (!vault) throw new Error("There is no vault to reset.")
  const inventory = await readInventory()
  const sealed = await vaultCrypto<{ envelope: string; scope_envelopes: string[] }>({
    action: "reset-vault",
    vault,
    password,
    key_epoch: inventory.key_epoch,
    inventory: inventory.scopes,
  })
  await commitReplacement(sealed.envelope, () =>
    post("environment-vault/reset?workspace=personal", {
      operation_id: operationId(),
      expected_vault_document_id: vault.document_id,
      vault_envelope: sealed.envelope,
      scope_envelopes: sealed.scope_envelopes,
      confirm_total_loss: true,
    }),
  )
}

export type { Vault }
