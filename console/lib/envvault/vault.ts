"use client"

import { api, ApiError, type Me } from "@/lib/api"
import { lockVault, vaultCrypto } from "@/lib/envvault/crypto"

/*
 * ENV in the browser, the same calls the CLI makes (`pb env set/list`) with the console's session:
 * the encrypted vault, one scope anchor, one record per variable, and a host layer for every
 * machine that has no key for the scope yet. Names are listed and values are written; no
 * function here can return a value.
 */

export type Vault = { issuer: string; account_id: string; generation: number; document_id: string; envelope: string }
/** Where variables live. `kind` "team" is the workspace's shared set; "personal" with a `machine` is a device override. */
export type EnvScope = { kind: "personal" | "team"; owner: string; machine: string; workspace: string }
type Scope = {
  workspace_id: string
  writer_public: string
  owner_kind: string
  owner_id: string
  machine_id: string
  key_epoch: number
  revision: number
  document_id: string
  envelope: string
}
type EnvRecord = {
  workspace_id: string
  owner_kind: string
  owner_id: string
  machine_id: string
  key_epoch: number
  record_id: string
  revision: number
  deleted: boolean
  sequence: number
  document_id: string
  writer_public: string
  envelope: string
}
type RecordsPage = { records: EnvRecord[]; sequence: number; after_sequence: number; next_cursor?: string }
type Recipient = {
  recipient_account: string
  machine_id: string
  installation_generation: number
  host_key_generation: number
  host_public: string
  delivery_generation: number
  document_id: string
  fence_generation: number
}
type RecipientsPage = {
  items: Recipient[]
  pagination: { limit: number; offset: number; total: number; next_offset: number | null }
}
type Team = { entitled?: boolean; team_id: string } & Record<string, unknown>

/** Some devices still need their key for this scope; the saved change reaches them after a retry. */
export class DeliveryPending extends Error {
  constructor(readonly failed: number) {
    super(
      `Saved. ${failed === 1 ? "One machine" : `${failed} machines`} will receive it after your next change, or run pb env vault resume.`,
    )
  }
}

const json = { "content-type": "application/json" }
const operationId = () => {
  const b = crypto.getRandomValues(new Uint8Array(16))
  b[6] = (b[6] & 0x0f) | 0x40
  b[8] = (b[8] & 0x3f) | 0x80
  const h = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("")
  return `operation_${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`
}
const query = (scope: EnvScope, extra: Record<string, string> = {}) =>
  new URLSearchParams({ workspace: scope.workspace, machine_id: scope.machine, ...extra })
const scopePath = (scope: EnvScope) => `environment/scopes/${scope.kind}/${encodeURIComponent(scope.owner)}`
const notFound = (error: unknown) => error instanceof ApiError && error.status === 404

/** The account's encrypted vault, or null before `pb env vault init` has created one. */
export async function readVault(): Promise<Vault | null> {
  try {
    return await api<Vault>("environment-vault?workspace=personal")
  } catch (error) {
    if (notFound(error)) return null
    throw error
  }
}

/** The password never leaves the browser: it opens the vault inside the worker. */
export async function unlockVault(password: string): Promise<void> {
  const [vault, me] = await Promise.all([readVault(), api<Me>("me")])
  if (!vault) throw new ApiError(404, "not_found", "No vault yet. Run pb env vault init on a machine first.")
  if (vault.account_id !== me.id) {
    lockVault()
    throw new Error("The ENV vault does not belong to the signed-in account.")
  }
  await vaultCrypto({ action: "unlock", vault, password })
}

async function currentVault(): Promise<Vault> {
  const vault = await readVault()
  if (!vault) throw new Error("No ENV vault exists for this account.")
  return vault
}

function sameScope(
  scope: EnvScope,
  item: { workspace_id: string; owner_kind: string; owner_id: string; machine_id: string },
) {
  return (
    item.workspace_id === scope.workspace &&
    item.owner_kind === scope.kind &&
    item.owner_id === scope.owner &&
    item.machine_id === scope.machine
  )
}

async function readScope(scope: EnvScope): Promise<Scope | null> {
  try {
    const anchor = await api<Scope>(`${scopePath(scope)}?${query(scope)}`)
    if (!sameScope(scope, anchor)) throw new Error("The encrypted scope did not match the one requested.")
    return anchor
  } catch (error) {
    if (notFound(error)) return null
    throw error
  }
}

async function readRecords(scope: EnvScope): Promise<EnvRecord[]> {
  const records: EnvRecord[] = []
  const seen = new Set<string>()
  let through: number | undefined
  let cursor: string | undefined
  for (let pages = 0; pages < 100; pages++) {
    const params = query(scope, { after_sequence: "0", limit: "200" })
    if (through !== undefined) params.set("through_sequence", String(through))
    if (cursor) params.set("cursor", cursor)
    const page = await api<RecordsPage>(`${scopePath(scope)}/records?${params}`)
    if (page.records.length > 200 || (through !== undefined && page.sequence !== through)) {
      throw new Error("ENV record paging was invalid. Nothing is shown.")
    }
    through ??= page.sequence
    for (const record of page.records) {
      if (!sameScope(scope, record) || seen.has(record.record_id) || record.sequence > through) {
        throw new Error("An encrypted ENV record did not match its scope.")
      }
      seen.add(record.record_id)
      records.push(record)
    }
    if (page.next_cursor === undefined) return records
    if (!page.next_cursor || page.records.length === 0 || page.next_cursor === cursor) {
      throw new Error("ENV record paging did not advance.")
    }
    cursor = page.next_cursor
  }
  throw new Error("Too many ENV records to list.")
}

/** Variable names in a scope, sorted. Needs the vault unlocked; values are never read out. */
export async function listNames(scope: EnvScope): Promise<string[]> {
  const vault = await currentVault()
  if (!(await readScope(scope))) return []
  const names: string[] = []
  for (const record of await readRecords(scope)) {
    const read = await vaultCrypto<{ name: string; deleted: boolean }>({ action: "record-name", vault, record })
    if (read.deleted !== record.deleted) throw new Error("An ENV record did not match its encrypted contents.")
    if (!read.deleted) names.push(read.name)
  }
  return names.sort()
}

const coordinates = (scope: EnvScope) => ({
  kind: scope.kind,
  owner: scope.owner,
  machine: scope.machine,
  workspace_id: scope.workspace,
})

async function ensureAnchor(scope: EnvScope, vault: Vault): Promise<Scope> {
  const existing = await readScope(scope)
  if (existing) return existing
  const { envelope } = await vaultCrypto<{ envelope: string }>({ action: "scope-create", vault, ...coordinates(scope) })
  try {
    const created = await api<Scope>(`${scopePath(scope)}?${query(scope)}`, {
      method: "PUT",
      headers: json,
      body: JSON.stringify({ operation_id: operationId(), envelope }),
    })
    if (!sameScope(scope, created)) throw new Error("The encrypted scope did not match the one requested.")
    return created
  } catch (error) {
    // A lost response may still have committed the anchor.
    const committed = await readScope(scope).catch(() => null)
    if (committed) return committed
    throw error
  }
}

async function readRecord(scope: EnvScope, id: string): Promise<EnvRecord | null> {
  try {
    const record = await api<EnvRecord>(`${scopePath(scope)}/records/${id}?${query(scope)}`)
    if (!sameScope(scope, record) || record.record_id !== id) throw new Error("An ENV record did not match its scope.")
    return record
  } catch (error) {
    if (notFound(error)) return null
    throw error
  }
}

async function recipients(scope: EnvScope): Promise<Recipient[]> {
  const items: Recipient[] = []
  for (let offset = 0, pages = 0; pages < 100; pages++) {
    const params = new URLSearchParams({
      workspace: scope.workspace,
      owner_kind: scope.kind,
      owner_id: scope.owner,
      machine_id: scope.machine,
      state: "pending",
      limit: "50",
      offset: String(offset),
    })
    const page = await api<RecipientsPage>(`environment/layers?${params}`)
    const p = page.pagination
    if (page.items.length > 50 || p.offset !== offset || p.total < offset + page.items.length) {
      throw new Error("ENV recipient paging was invalid.")
    }
    items.push(...page.items)
    if (p.next_offset === null) {
      if (p.total !== offset + page.items.length) throw new Error("The ENV recipient list was incomplete.")
      return items
    }
    if (page.items.length === 0 || p.next_offset !== offset + page.items.length) {
      throw new Error("ENV recipient paging did not advance.")
    }
    offset = p.next_offset
  }
  throw new Error("Too many ENV recipients.")
}

/**
 * Like the CLI after every set: each machine allowed to receive this scope but holding no key
 * for it gets one, sealed in the worker to that machine's registered key. Machines that already
 * have the key read new records themselves at their next launch, so most edits seal nothing.
 */
async function deliver(scope: EnvScope, vault: Vault, anchor: Scope): Promise<void> {
  const pending = await recipients(scope)
  if (pending.length === 0) return
  const team =
    scope.kind === "team"
      ? await api<Team>(
          `environment/teams/${encodeURIComponent(scope.owner)}?workspace=${encodeURIComponent(scope.workspace)}`,
        )
      : undefined
  if (team && team.entitled === false) {
    throw new ApiError(
      403,
      "team_entitlement_required",
      "An active Team plan is needed to send Team variables to machines.",
    )
  }
  let failed = 0
  for (const recipient of pending) {
    try {
      const layer = await vaultCrypto<{ operation_id: string; envelope: string }>({
        action: "host-layer",
        vault,
        scope: anchor,
        workspace_id: scope.workspace,
        recipient_metadata: recipient,
        operation_id: operationId(),
        ...(team ? { team } : {}),
      })
      await api(
        `environment/layers/${encodeURIComponent(recipient.machine_id)}?workspace=${encodeURIComponent(scope.workspace)}`,
        {
          method: "PUT",
          headers: json,
          body: JSON.stringify({ operation_id: layer.operation_id, envelope: layer.envelope }),
        },
      )
    } catch {
      failed++
    }
  }
  if (failed) throw new DeliveryPending(failed)
}

async function writeRecord(scope: EnvScope, name: string, value: string, deleted: boolean): Promise<void> {
  const vault = await currentVault()
  const { record_id: id } = await vaultCrypto<{ record_id: string }>({
    action: "record-id",
    vault,
    ...coordinates(scope),
    name,
  })
  const anchor = deleted ? await readScope(scope) : await ensureAnchor(scope, vault)
  const current = anchor ? await readRecord(scope, id) : null
  if (deleted && (!anchor || !current || current.deleted)) throw new Error(`${name} is not set.`)
  const sealed = await vaultCrypto<{ expected_revision: number; envelope: string }>({
    action: "record-write",
    vault,
    scope: anchor,
    ...(current ? { record: current } : {}),
    ...coordinates(scope),
    name,
    value,
    deleted,
  })
  try {
    await api(`${scopePath(scope)}/records?${query(scope)}`, {
      method: "PUT",
      headers: json,
      body: JSON.stringify({ operation_id: operationId(), records: [sealed] }),
    })
  } catch (error) {
    if (error instanceof ApiError && error.status === 409) {
      throw new Error("This variable changed elsewhere. Reload and try again.")
    }
    throw error
  }
  if (anchor) await deliver(scope, vault, anchor)
}

export const setValue = (scope: EnvScope, name: string, value: string) => writeRecord(scope, name, value, false)
export const deleteValue = (scope: EnvScope, name: string) => writeRecord(scope, name, "", true)
