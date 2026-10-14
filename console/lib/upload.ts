"use client"

import { ApiError, idempotencyKey } from "@/lib/api"

/**
 * Sends one file as the raw request body and reports progress (fetch can't report upload
 * progress; XMLHttpRequest can). The relay streams these bodies straight through
 * (app/api/v1/[...path]/route.ts). Answers with the response's `data`.
 */
export function uploadFile<T>(
  path: string,
  file: File,
  onProgress: (fraction: number) => void,
): { done: Promise<T>; cancel: () => void } {
  const request = new XMLHttpRequest()
  const done = new Promise<T>((resolve, reject) => {
    request.upload.onprogress = (event) => event.lengthComputable && onProgress(event.loaded / event.total)
    request.onload = () => {
      let body: { data?: T; error?: { code?: string; message?: string } } | null = null
      try {
        body = JSON.parse(request.responseText)
      } catch {
        // Not JSON: reported below as unavailable.
      }
      if (request.status >= 200 && request.status < 300 && body?.data !== undefined) resolve(body.data)
      else
        reject(
          new ApiError(
            request.status,
            body?.error?.code ?? "unavailable",
            body?.error?.message ?? "Paperboat is temporarily unavailable.",
          ),
        )
    }
    request.onerror = () =>
      reject(new ApiError(0, "network", "The upload was interrupted. Check your connection and try again."))
    request.onabort = () => reject(new ApiError(0, "canceled", "Upload canceled."))
  })
  request.open("POST", `/api/v1/${path}`)
  request.setRequestHeader("accept", "application/json")
  request.setRequestHeader("idempotency-key", idempotencyKey())
  request.setRequestHeader("content-type", file.type || "application/octet-stream")
  request.send(file)
  return { done, cancel: () => request.abort() }
}
