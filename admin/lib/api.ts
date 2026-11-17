"use client"

import { useQuery, type QueryKey, type UseQueryResult } from "@tanstack/react-query"
import type { components, paths } from "@/lib/api-schema"
import { withDeadline } from "@/lib/deadline"

/** Server schemas, generated from server/docs/openapi.json (`bun run api:types`). */
export type Schemas = components["schemas"]

/** The `data` of a path's successful JSON response, as the server documents it. */
export type Data<P extends keyof paths> = paths[P] extends { get: { responses: infer R } }
  ? R extends { 200: { content: { "application/json": { data?: infer T } } } }
    ? NonNullable<T>
    : never
  : never

/** What `/v1/operator/me` says about the signed-in operator. */
export type Operator = Data<"/v1/operator/me">
export type MfaState = Operator["mfa"]

/**
 * Error envelope of the control plane: `{ error: { code, message, request_id, support_reference } }`. Every
 * failure the panel shows carries `reference`, the id support can search for.
 */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    /** support_reference, else request_id: what to quote when asking about this failure. */
    readonly reference?: string,
  ) {
    super(message)
  }
}

const readTimeoutMs = 30_000

/** GET a control-plane path through the panel's relay; failures are ApiErrors with a reference. */
export async function api<T>(path: string, signal?: AbortSignal): Promise<T> {
  let response: Response
  try {
    response = await fetch(`/api${path}`, { signal: withDeadline(signal, readTimeoutMs), headers: { accept: "application/json" } })
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") throw error
    const timeout = error instanceof DOMException && error.name === "TimeoutError"
    throw new ApiError(0, timeout ? "timeout" : "network", timeout ? "The server took too long to answer." : "Couldn’t reach the panel server.")
  }
  const reference = response.headers.get("support-reference") ?? response.headers.get("request-id") ?? undefined
  if (!response.ok) {
    let code = "error"
    let message = `The server answered ${response.status}.`
    try {
      const body = (await response.json()) as { error?: { code?: string; message?: string; support_reference?: string; request_id?: string } }
      code = body.error?.code ?? code
      message = body.error?.message ?? message
      throw new ApiError(response.status, code, message, body.error?.support_reference ?? body.error?.request_id ?? reference)
    } catch (error) {
      if (error instanceof ApiError) throw error
      throw new ApiError(response.status, code, message, reference)
    }
  }
  const body = (await response.json()) as { data: T }
  return body.data
}

export function useApiQuery<T>(key: QueryKey, path: string | null, refetchMs?: number): UseQueryResult<T, ApiError> {
  return useQuery<T, ApiError>({
    queryKey: key,
    queryFn: ({ signal }) => api<T>(path as string, signal),
    enabled: path !== null,
    refetchInterval: refetchMs,
  })
}

type Envelope<T> = { data?: T; error?: { code?: string; message?: string; support_reference?: string; request_id?: string } }

/** Send JSON through the relay (sign-in, MFA, operator actions); failures are ApiErrors with a reference. */
export async function send<T>(method: "POST" | "PUT" | "DELETE", path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
  let response: Response
  try {
    response = await fetch(`/api${path}`, {
      method,
      headers: { "content-type": "application/json", accept: "application/json", ...headers },
      body: body === undefined ? undefined : JSON.stringify(body),
    })
  } catch {
    throw new ApiError(0, "network", "Couldn’t reach the panel server. Check the target before retrying: the action may have been applied.")
  }
  const reference = response.headers.get("support-reference") ?? response.headers.get("request-id") ?? undefined
  if (response.status === 204) return undefined as T
  const parsed = (await response.json().catch(() => ({}))) as Envelope<T>
  if (!response.ok) {
    throw new ApiError(
      response.status,
      parsed.error?.code ?? "error",
      parsed.error?.message ?? `The server answered ${response.status}.`,
      parsed.error?.support_reference ?? parsed.error?.request_id ?? reference,
    )
  }
  return parsed.data as T
}

export const post = <T>(path: string, body?: unknown) => send<T>("POST", path, body)

/** What an operator write names: the target it acts on, the opaque reference for the audit log, and the retry key. */
export type ActionHeaders = { target: string; reason: string; key: string }

/** An operator write: the server requires the target, a reason reference and an idempotency key on every one. */
export const operatorAction = <T>(method: "POST" | "PUT" | "DELETE", path: string, h: ActionHeaders, body?: unknown) =>
  send<T>(method, path, body, { "operator-target": h.target, "operator-reason": h.reason, "idempotency-key": h.key })

/** The reference charset the server accepts for Operator-Reason and keys. */
export const referencePattern = /^[A-Za-z0-9_.:-]{1,128}$/

export const operatorQuery = { key: ["operator"] as const, path: "/v1/operator/me" }

/** The signed-in operator (cached by the shell); undefined while loading. */
export function useOperator(): Operator | undefined {
  return useApiQuery<Operator>(operatorQuery.key, operatorQuery.path).data
}

/** Relay, tunnel, hostname and control-operation writes need the operations role (or admin): the server refuses others. */
export function useCanOperate(): boolean {
  const role = useOperator()?.role
  return role === "admin" || role === "operations"
}
