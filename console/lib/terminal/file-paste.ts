import type { BrowserTerminalAPI } from "./browser-terminal-wasm"

export const FILE_TRANSFER_CHUNK_BYTES = 32 * 1024
export const FILE_TRANSFER_MAX_FILE_BYTES = 50 * 1024 * 1024
export const FILE_TRANSFER_MAX_BATCH_FILES = 10
export const FILE_TRANSFER_MAX_BATCH_BYTES = 500 * 1024 * 1024
const FILE_TRANSFER_RECONNECT_MS = 30_000
const FILE_TRANSFER_CLEANUP_MS = 12_000
const FILE_TRANSFER_MAX_RECOVERY_MS = 600_000
const FILE_TRANSFER_MAX_RETRIES = 2

export type FilePastePolicy = {
  max_file_bytes: number
  max_batch_files: number
  max_batch_bytes: number
  retention_seconds?: number
  delivery_timeout_seconds?: number
}

export type FilePasteProgress = {
  phase: "checking" | "hashing" | "uploading" | "finishing" | "canceling"
  fileNumber: number
  fileCount: number
  transferredBytes: number
  totalBytes: number
}

export type FilePasteConnection = {
  api: BrowserTerminalAPI
  connectionID: string
}

export type FilePasteSelection =
  { kind: "files"; files: File[] } | { kind: "file-path" } | { kind: "unavailable-file" } | { kind: "text" }

type TransferManifest = {
  transfer_id: string
  basename: string
  size: number
  sha256: string
  committed_offset: number
  state: string
  receipt_path?: string
}

type PreparedFile = {
  file: File
  sha256: string
  transferID?: string
}

type FilePasteOperation = {
  controller: AbortController
  batchID: string
  prepared: PreparedFile[]
  policy?: FilePastePolicy
  createStarted: boolean
  completed: boolean
  cancelPromise?: Promise<boolean>
  settled: Promise<void>
  settle: () => void
  recoveryDeadline?: number
  reconnectDeadline?: number
  cleanupDeadline?: number
  cleanupSucceeded?: boolean
}

export class FilePasteError extends Error {
  readonly code: string
  readonly failedConnectionID?: string
  cleanupFailed = false

  constructor(code: string, failedConnectionID?: string) {
    super(code)
    this.name = "FilePasteError"
    this.code = code
    this.failedConnectionID = failedConnectionID
  }
}

function record(value: unknown): Record<string, unknown> | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null
}

function manifest(value: unknown): TransferManifest {
  const item = record(value)
  if (
    !item ||
    typeof item.transfer_id !== "string" ||
    !item.transfer_id ||
    typeof item.basename !== "string" ||
    !Number.isSafeInteger(item.size) ||
    (item.size as number) < 0 ||
    !/^[0-9a-f]{64}$/.test(String(item.sha256 ?? "")) ||
    !Number.isSafeInteger(item.committed_offset) ||
    (item.committed_offset as number) < 0 ||
    (item.committed_offset as number) > (item.size as number) ||
    typeof item.state !== "string"
  ) {
    throw new FilePasteError("invalid_response")
  }
  return item as unknown as TransferManifest
}

function parsePolicy(value: unknown): FilePastePolicy {
  const container = record(value)
  const policy = record(container?.policy)
  if (
    !policy ||
    !Number.isSafeInteger(policy.max_file_bytes) ||
    (policy.max_file_bytes as number) < 1 ||
    !Number.isSafeInteger(policy.max_batch_files) ||
    (policy.max_batch_files as number) < 1 ||
    !Number.isSafeInteger(policy.max_batch_bytes) ||
    (policy.max_batch_bytes as number) < 1 ||
    (policy.max_batch_bytes as number) < (policy.max_file_bytes as number)
  ) {
    throw new FilePasteError("invalid_policy")
  }
  return {
    max_file_bytes: Math.min(policy.max_file_bytes as number, FILE_TRANSFER_MAX_FILE_BYTES),
    max_batch_files: Math.min(policy.max_batch_files as number, FILE_TRANSFER_MAX_BATCH_FILES),
    max_batch_bytes: Math.min(policy.max_batch_bytes as number, FILE_TRANSFER_MAX_BATCH_BYTES),
    retention_seconds: Number.isSafeInteger(policy.retention_seconds)
      ? (policy.retention_seconds as number)
      : undefined,
    delivery_timeout_seconds: Number.isSafeInteger(policy.delivery_timeout_seconds)
      ? (policy.delivery_timeout_seconds as number)
      : undefined,
  }
}

function errorCode(value: unknown): string {
  if (value instanceof FilePasteError) return value.code
  if (value && typeof value === "object" && "error" in value && typeof value.error === "string") return value.error
  if (value instanceof Error) {
    if (/worker stopped|worker could not complete|connection closed|connection was closed/i.test(value.message))
      return "connection_closed"
    if (/timeout|timed out/i.test(value.message)) return "file_timeout"
  }
  return "file_transfer_failed"
}

function isTransient(code: string): boolean {
  return (
    code === "connection_closed" ||
    code === "file_timeout" ||
    code === "credential_expired" ||
    code === "reconnect_timeout"
  )
}

function isFileURLList(value: string): boolean {
  return value.split(/\r?\n/).some((line) => {
    const candidate = line.trim()
    if (!candidate || candidate.startsWith("#")) return false
    try {
      return new URL(candidate).protocol === "file:"
    } catch {
      return /^file:/i.test(candidate)
    }
  })
}

function fileItems(data: Pick<DataTransfer, "files" | "items">): { files: File[]; hasFileItem: boolean } {
  const fromItems: File[] = []
  let hasFileItem = false
  for (const item of Array.from(data.items ?? [])) {
    if (item.kind !== "file") continue
    hasFileItem = true
    const file = item.getAsFile()
    if (file) fromItems.push(file)
  }
  if (fromItems.length) return { files: fromItems, hasFileItem }
  const files = Array.from(data.files ?? [])
  return { files, hasFileItem: hasFileItem || files.length > 0 }
}

export function classifyFilePaste(data: Pick<DataTransfer, "files" | "items" | "getData"> | null): FilePasteSelection {
  if (!data) return { kind: "text" }
  const found = fileItems(data)
  if (found.files.length) return { kind: "files", files: found.files }
  if (found.hasFileItem) return { kind: "unavailable-file" }
  let uriList = ""
  try {
    uriList = data.getData("text/uri-list")
  } catch {
    /* Browsers may deny access to optional clipboard formats. */
  }
  return isFileURLList(uriList) ? { kind: "file-path" } : { kind: "text" }
}

function isWindowsPath(path: string): boolean {
  return /^[a-zA-Z]:[\\/]/.test(path) || path.startsWith("\\\\")
}

export function quoteRemotePath(path: string): string {
  const windows = isWindowsPath(path)
  if (/[\0\r\n]/.test(path) || (!windows && !path.startsWith("/"))) {
    throw new FilePasteError("unsafe_remote_path")
  }
  if (windows && /["$`%!^]/.test(path)) throw new FilePasteError("unsafe_remote_path")
  const safe = /^[a-zA-Z0-9/_\-.:]+$/.test(path) || (windows && /^[a-zA-Z0-9/_\-.:\\]+$/.test(path))
  if (safe) return path
  if (windows) return `"${path}"`
  return `'${path.replace(/'/g, `'"'"'`)}'`
}

export function quoteRemotePaths(paths: string[]): string {
  if (!paths.length) throw new FilePasteError("invalid_response")
  return paths.map(quoteRemotePath).join(" ")
}

export function filePasteMessage(code: string): string {
  switch (code) {
    case "owner_only":
      return "Only the terminal owner can upload files. Ask the owner to upload it or arrange machine access."
    case "file-path":
      return "The clipboard only contains a local file path, so Paperboat cannot read its contents. Choose the file here or drop it into the terminal."
    case "unavailable-file":
      return "The clipboard did not provide file contents. Choose the file here or drop it into the terminal."
    case "not_connected":
      return "Reconnect the terminal before uploading files."
    case "busy":
      return "A file transfer is already running. Wait for it to finish or cancel it first."
    case "capability_disabled":
      return "File transfer is unavailable for this terminal. Check that the machine's file transfer capability is enabled."
    case "capability_required":
      return "Update Paperboat on this machine to upload files from the web terminal."
    case "file_busy":
      return "The machine is handling another file request. Wait a moment and try again."
    case "file_limit":
    case "invalid_size":
    case "batch_limit":
      return "The selected files exceed this machine's file transfer limits. Choose fewer or smaller files."
    case "hash_failed":
    case "file_read_failed":
      return "The selected file could not be read. Choose it again and retry."
    case "digest_mismatch":
      return "The machine could not verify the uploaded files. Choose them again to retry."
    case "resource_limit":
    case "storage_unavailable":
      return "The machine cannot receive files right now. Try again later."
    case "unsafe_remote_path":
      return "The files uploaded, but a remote path needs shell-specific quoting, so no paths were pasted. Open the machine's Paperboat Inbox to locate them."
    case "not_found_or_forbidden":
    case "stale_generation":
      return "The terminal changed or closed during upload. Reopen the current terminal and choose the files again."
    case "canceled":
      return "The file transfer was canceled."
    case "reconnect_timeout":
    case "connection_closed":
    case "file_timeout":
    case "credential_expired":
      return "The terminal did not reconnect in time. Reconnect and choose the files again."
    default:
      return "The file transfer could not finish. Reconnect and choose the files again; no paths were pasted."
  }
}

function createBatchID(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 0x0f) | 0x40
  bytes[8] = (bytes[8] & 0x3f) | 0x80
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("")
  return `browser_${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

function standardBase64(bytes: Uint8Array): string {
  let binary = ""
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(offset, Math.min(offset + 0x8000, bytes.length)))
  }
  return btoa(binary)
}

async function sha256Hex(bytes: Uint8Array): Promise<string> {
  const ownedBuffer = new ArrayBuffer(bytes.byteLength)
  new Uint8Array(ownedBuffer).set(bytes)
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", ownedBuffer))
  return Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("")
}

function checkAborted(operation: FilePasteOperation): void {
  if (operation.controller.signal.aborted) throw new FilePasteError("canceled")
}

export class FilePasteUploader {
  private active: FilePasteOperation | null = null
  private readonly connection: () => FilePasteConnection | null
  private readonly onProgress: (progress: FilePasteProgress | null) => void
  private readonly onCleanupFailure: () => void

  constructor(
    connection: () => FilePasteConnection | null,
    onProgress: (progress: FilePasteProgress | null) => void,
    onCleanupFailure: () => void = () => undefined,
  ) {
    this.connection = connection
    this.onProgress = onProgress
    this.onCleanupFailure = onCleanupFailure
  }

  async upload(files: File[]): Promise<string> {
    if (this.active) throw new FilePasteError("busy")
    if (!files.length) throw new FilePasteError("invalid_request")
    let settle!: () => void
    const settled = new Promise<void>((resolve) => {
      settle = resolve
    })
    const operation: FilePasteOperation = {
      controller: new AbortController(),
      batchID: createBatchID(),
      prepared: [],
      createStarted: false,
      completed: false,
      settled,
      settle,
    }
    this.active = operation
    const chosen = [...files]
    const totalBytes = chosen.reduce((total, file) => total + file.size, 0)
    try {
      this.onProgress({ phase: "checking", fileNumber: 0, fileCount: chosen.length, transferredBytes: 0, totalBytes })
      const policy = parsePolicy(await this.request(operation, { action: "policy" }, "retry"))
      operation.policy = policy
      validateBatch(chosen, policy)
      checkAborted(operation)

      for (let index = 0; index < chosen.length; index++) {
        const file = chosen[index]
        this.onProgress({
          phase: "hashing",
          fileNumber: index + 1,
          fileCount: chosen.length,
          transferredBytes: 0,
          totalBytes,
        })
        const activeConnection = await this.waitForConnection(operation)
        let hashed: Awaited<ReturnType<BrowserTerminalAPI["hashFile"]>>
        try {
          hashed = await activeConnection.api.hashFile(file)
        } catch {
          throw new FilePasteError("hash_failed")
        }
        checkAborted(operation)
        if ("error" in hashed) throw new FilePasteError(hashed.error)
        if (!/^[0-9a-f]{64}$/.test(hashed.sha256)) throw new FilePasteError("invalid_response")
        operation.prepared.push({ file, sha256: hashed.sha256 })
      }

      checkAborted(operation)
      const createRequest = {
        action: "create",
        batch_id: operation.batchID,
        files: operation.prepared.map(({ file, sha256 }) => ({ basename: file.name, size: file.size, sha256 })),
      }
      operation.createStarted = true
      const created = await this.request(operation, createRequest, "retry")
      const container = record(created)
      if (!Array.isArray(container?.transfers) || container.transfers.length !== operation.prepared.length) {
        throw new FilePasteError("invalid_response")
      }
      for (let index = 0; index < operation.prepared.length; index++) {
        const transfer = manifest(container.transfers[index])
        const prepared = operation.prepared[index]
        if (
          transfer.basename !== prepared.file.name ||
          transfer.size !== prepared.file.size ||
          transfer.sha256 !== prepared.sha256
        ) {
          throw new FilePasteError("invalid_response")
        }
        prepared.transferID = transfer.transfer_id
      }

      let completedBytes = 0
      for (let index = 0; index < operation.prepared.length; index++) {
        const prepared = operation.prepared[index]
        const transferID = prepared.transferID!
        const current = await this.getTransfer(operation, transferID)
        if (current.state === "published") {
          completedBytes += prepared.file.size
          continue
        }
        this.assertWritable(current)
        let offset = current.committed_offset
        completedBytes += offset
        this.onProgress({
          phase: "uploading",
          fileNumber: index + 1,
          fileCount: chosen.length,
          transferredBytes: completedBytes,
          totalBytes,
        })
        let retriesAtOffset = 0
        while (offset < prepared.file.size) {
          checkAborted(operation)
          const end = Math.min(offset + FILE_TRANSFER_CHUNK_BYTES, prepared.file.size)
          let bytes: Uint8Array
          try {
            bytes = new Uint8Array(await prepared.file.slice(offset, end).arrayBuffer())
          } catch {
            throw new FilePasteError("file_read_failed")
          }
          if (bytes.byteLength !== end - offset || !bytes.byteLength) throw new FilePasteError("file_read_failed")
          const chunkRequest = {
            action: "chunk",
            transfer_id: transferID,
            offset,
            data: standardBase64(bytes),
            sha256: await sha256Hex(bytes),
          }
          try {
            const appended = record(await this.request(operation, chunkRequest, "single"))
            const after = manifest(appended?.transfer)
            if (after.transfer_id !== transferID || after.committed_offset !== end)
              throw new FilePasteError("invalid_response")
            offset = end
            retriesAtOffset = 0
            completedBytes += bytes.byteLength
            this.onProgress({
              phase: "uploading",
              fileNumber: index + 1,
              fileCount: chosen.length,
              transferredBytes: completedBytes,
              totalBytes,
            })
          } catch (reason) {
            const code = errorCode(reason)
            if (!isTransient(code)) throw reason
            const failedConnectionID = reason instanceof FilePasteError ? reason.failedConnectionID : undefined
            const recovered = await this.getTransfer(operation, transferID, failedConnectionID)
            this.assertWritable(recovered)
            if (recovered.committed_offset < offset || recovered.committed_offset > end)
              throw new FilePasteError("invalid_response")
            if (recovered.committed_offset === end) {
              offset = end
              retriesAtOffset = 0
              completedBytes += bytes.byteLength
              this.onProgress({
                phase: "uploading",
                fileNumber: index + 1,
                fileCount: chosen.length,
                transferredBytes: completedBytes,
                totalBytes,
              })
            } else {
              retriesAtOffset++
              if (retriesAtOffset > FILE_TRANSFER_MAX_RETRIES) throw new FilePasteError("connection_closed")
              offset = recovered.committed_offset
            }
          }
        }
      }

      checkAborted(operation)
      this.onProgress({
        phase: "finishing",
        fileNumber: chosen.length,
        fileCount: chosen.length,
        transferredBytes: totalBytes,
        totalBytes,
      })
      const firstID = operation.prepared[0].transferID!
      const completion = await this.complete(operation, firstID)
      const first = manifest(record(completion)?.transfer)
      if (first.transfer_id !== firstID || first.state !== "published" || !first.receipt_path) {
        throw new FilePasteError("invalid_response")
      }
      const paths: string[] = [first.receipt_path]
      for (const prepared of operation.prepared.slice(1)) {
        const transferID = prepared.transferID!
        const completed = await this.complete(operation, transferID)
        const transfer = manifest(record(completed)?.transfer)
        if (transfer.transfer_id !== transferID || transfer.state !== "published" || !transfer.receipt_path)
          throw new FilePasteError("invalid_response")
        paths.push(transfer.receipt_path)
      }
      checkAborted(operation)
      operation.completed = true
      return quoteRemotePaths(paths)
    } catch (reason) {
      const code = errorCode(reason)
      let cleanupFailed = false
      if ((operation.createStarted || operation.prepared.some((item) => item.transferID)) && !operation.completed) {
        const cleaned = await this.cleanup(operation)
        cleanupFailed = !cleaned
        if (cleanupFailed) this.onCleanupFailure()
      }
      const failure = reason instanceof FilePasteError ? reason : new FilePasteError(code)
      failure.cleanupFailed ||= cleanupFailed
      throw failure
    } finally {
      if (this.active === operation) this.active = null
      operation.settle()
      this.onProgress(null)
    }
  }

  async cancel(): Promise<boolean> {
    const operation = this.active
    if (!operation) return true
    operation.controller.abort()
    this.onProgress({
      phase: "canceling",
      fileNumber: 0,
      fileCount: operation.prepared.length,
      transferredBytes: 0,
      totalBytes: operation.prepared.reduce((sum, item) => sum + item.file.size, 0),
    })
    if (operation.prepared.some((item) => item.transferID)) void this.cleanup(operation)
    await operation.settled
    return operation.cleanupSucceeded ?? !operation.createStarted
  }

  dispose(): Promise<boolean> {
    const operation = this.active
    if (!operation) return Promise.resolve(true)
    operation.controller.abort()
    if (operation.prepared.some((item) => item.transferID)) return this.cleanup(operation)
    if (operation.createStarted) return operation.settled.then(() => operation.cleanupSucceeded ?? false)
    return Promise.resolve(true)
  }

  private async complete(operation: FilePasteOperation, transferID: string): Promise<unknown> {
    try {
      return await this.request(operation, { action: "complete", transfer_id: transferID }, "retry")
    } catch (reason) {
      if (!isTransient(errorCode(reason))) throw reason
      const current = await this.getTransfer(
        operation,
        transferID,
        reason instanceof FilePasteError ? reason.failedConnectionID : undefined,
      )
      if (current.state === "published" && current.receipt_path) return { transfer: current }
      return this.request(operation, { action: "complete", transfer_id: transferID }, "retry")
    }
  }

  private async getTransfer(
    operation: FilePasteOperation,
    transferID: string,
    previousConnectionID?: string,
  ): Promise<TransferManifest> {
    const value = record(
      await this.request(operation, { action: "get", transfer_id: transferID }, "retry", false, previousConnectionID),
    )
    const transfer = manifest(value?.transfer)
    if (transfer.transfer_id !== transferID) throw new FilePasteError("invalid_response")
    return transfer
  }

  private assertWritable(transfer: TransferManifest): void {
    if (transfer.state !== "created" && transfer.state !== "uploading")
      throw new FilePasteError(transfer.state === "canceled" ? "canceled" : "state_conflict")
  }

  private async request(
    operation: FilePasteOperation,
    payload: Record<string, unknown>,
    retry: "single" | "retry",
    cleanup = false,
    previousConnectionID?: string,
  ): Promise<unknown> {
    let previous = previousConnectionID
    for (let attempt = 0; attempt <= (retry === "retry" ? FILE_TRANSFER_MAX_RETRIES : 0); attempt++) {
      if (!cleanup) checkAborted(operation)
      let connection: FilePasteConnection | null = null
      try {
        connection = await this.waitForConnection(operation, previous, cleanup)
        const response = await connection.api.fileRequest(connection.connectionID, payload)
        if (response.error) {
          if (!isTransient(response.error)) throw new FilePasteError(response.error)
          if (attempt >= (retry === "retry" ? FILE_TRANSFER_MAX_RETRIES : 0))
            throw new FilePasteError(response.error, connection.connectionID)
          previous = connection.connectionID
          this.armReconnect(operation, cleanup)
          continue
        }
        if (!Object.prototype.hasOwnProperty.call(response, "result")) throw new FilePasteError("invalid_response")
        return response.result
      } catch (reason) {
        const code = errorCode(reason)
        if (reason instanceof FilePasteError && !isTransient(code)) throw reason
        if (!isTransient(code) && code !== "file_transfer_failed") throw reason
        if (attempt >= (retry === "retry" ? FILE_TRANSFER_MAX_RETRIES : 0))
          throw new FilePasteError(isTransient(code) ? code : "connection_closed", connection?.connectionID)
        if (connection) previous = connection.connectionID
        this.armReconnect(operation, cleanup)
      }
    }
    throw new FilePasteError("connection_closed")
  }

  private async waitForConnection(
    operation: FilePasteOperation,
    previousConnectionID?: string,
    cleanup = false,
  ): Promise<FilePasteConnection> {
    const current = this.connection()
    if (
      current?.api &&
      current.connectionID &&
      (!previousConnectionID || current.connectionID !== previousConnectionID)
    ) {
      operation.reconnectDeadline = undefined
      return current
    }
    if (cleanup) operation.cleanupDeadline ??= Date.now() + FILE_TRANSFER_CLEANUP_MS
    else this.armReconnect(operation, false)
    const deadline = cleanup ? operation.cleanupDeadline! : operation.reconnectDeadline!
    while (Date.now() < deadline) {
      if (!cleanup) checkAborted(operation)
      const next = this.connection()
      if (next?.api && next.connectionID && (!previousConnectionID || next.connectionID !== previousConnectionID)) {
        operation.reconnectDeadline = undefined
        return next
      }
      await new Promise((resolve) => setTimeout(resolve, 50))
    }
    operation.reconnectDeadline = undefined
    throw new FilePasteError("reconnect_timeout")
  }

  private armReconnect(operation: FilePasteOperation, cleanup: boolean): void {
    const now = Date.now()
    if (cleanup) {
      operation.cleanupDeadline ??= now + FILE_TRANSFER_CLEANUP_MS
      return
    }
    if (operation.recoveryDeadline === undefined) {
      const policyWait = operation.policy?.delivery_timeout_seconds
      const recoveryMS =
        Number.isSafeInteger(policyWait) && (policyWait as number) > 0
          ? Math.min((policyWait as number) * 1000, FILE_TRANSFER_MAX_RECOVERY_MS)
          : FILE_TRANSFER_MAX_RECOVERY_MS
      operation.recoveryDeadline = now + recoveryMS
    }
    operation.reconnectDeadline = Math.min(now + FILE_TRANSFER_RECONNECT_MS, operation.recoveryDeadline)
  }

  private cleanup(operation: FilePasteOperation): Promise<boolean> {
    if (operation.completed) return Promise.resolve(true)
    if (operation.cancelPromise) return operation.cancelPromise
    const transferID = operation.prepared.find((item) => item.transferID)?.transferID
    if (!transferID) {
      operation.cleanupSucceeded = !operation.createStarted
      return Promise.resolve(operation.cleanupSucceeded)
    }
    operation.cancelPromise = this.request(operation, { action: "cancel", transfer_id: transferID }, "retry", true)
      .then(() => {
        operation.cleanupSucceeded = true
        return true
      })
      .catch(() => {
        operation.cleanupSucceeded = false
        return false
      })
    return operation.cancelPromise
  }
}

function validateBatch(files: File[], policy: FilePastePolicy): void {
  if (
    !Number.isSafeInteger(policy.max_file_bytes) ||
    !Number.isSafeInteger(policy.max_batch_bytes) ||
    !Number.isSafeInteger(policy.max_batch_files) ||
    files.length > policy.max_batch_files
  ) {
    throw new FilePasteError("batch_limit")
  }
  let total = 0
  for (const file of files) {
    if (!Number.isSafeInteger(file.size) || file.size < 0 || file.size > policy.max_file_bytes)
      throw new FilePasteError("invalid_size")
    total += file.size
    if (!Number.isSafeInteger(total) || total > policy.max_batch_bytes) throw new FilePasteError("batch_limit")
  }
}
