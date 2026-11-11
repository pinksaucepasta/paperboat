"use client"

/*
 * The browser vault's cryptography is the CLI's own Go code (cli/cmd/environmentvault-wasm) in a
 * worker. The vault keys exist only in that worker's WASM memory; terminating the worker is the
 * lock. Nothing here returns a variable's value: the module can seal one, never show one.
 */

type Reply = { data?: unknown; error?: string }
type Waiting = { resolve: (reply: Reply) => void; reject: (error: Error) => void }

const idleLockMs = 10 * 60_000

let worker: Worker | undefined
let unlocked = false
let sequence = 0
let idle: ReturnType<typeof setTimeout> | undefined
const waiting = new Map<number, Waiting>()
const listeners = new Set<() => void>()

function notify() {
  listeners.forEach((listener) => listener())
}

export function subscribeVaultLock(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export const vaultUnlocked = () => unlocked

/** Drops every key: the worker and its memory go away, and pending calls fail. */
export function lockVault() {
  if (idle) clearTimeout(idle)
  worker?.terminate()
  worker = undefined
  waiting.forEach(({ reject }) => reject(new Error("The ENV vault was locked.")))
  waiting.clear()
  if (unlocked) {
    unlocked = false
    notify()
  }
}

function start(): Worker {
  if (worker) return worker
  const next = new Worker("/environment-vault/worker.js", { name: "paperboat-environment-vault" })
  next.onmessage = ({ data }: MessageEvent<{ id: number; reply: string }>) => {
    const call = waiting.get(data.id)
    waiting.delete(data.id)
    if (call) call.resolve(JSON.parse(data.reply) as Reply)
  }
  next.onerror = () => {
    waiting.forEach(({ reject }) => reject(new Error("ENV cryptography could not load. Reload the page.")))
    waiting.clear()
    worker = undefined
  }
  worker = next
  window.addEventListener("pagehide", lockVault, { once: true })
  return next
}

/** One vault operation. Any use restarts the idle timer; unlocking marks the vault open. */
export async function vaultCrypto<T>(request: { action: string } & Record<string, unknown>): Promise<T> {
  const target = start()
  const id = ++sequence
  const reply = await new Promise<Reply>((resolve, reject) => {
    waiting.set(id, { resolve, reject })
    target.postMessage({ id, request: JSON.stringify(request) })
  })
  if (reply.error || reply.data === undefined) throw new Error(reply.error ?? "The ENV operation failed.")
  if (request.action === "unlock") {
    unlocked = true
    notify()
  }
  if (unlocked) {
    if (idle) clearTimeout(idle)
    idle = setTimeout(lockVault, idleLockMs)
  }
  return reply.data as T
}
