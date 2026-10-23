"use client"

import { queryOptions } from "@tanstack/react-query"
import { api } from "@/lib/api"

/*
 * Connecting GitHub for config sync. `pb config` (or Config sync here) starts it; this console
 * reviews and approves the request, GitHub returns to the server, and the server sends the browser
 * to /github/done. The API answers JSON only. Contract: Lane B, 2026-10-23 (PLAN Log).
 */

/** The console-based GitHub flow (server 2026.10.23.37, 30a69c07). */
export const githubLive = true

export type GitHubStatus = {
  connected: boolean
  /** The connected GitHub account. */
  login?: string
  avatar_url?: string
  scopes: string[]
  missing_scopes: string[]
  config_repo_provisioned: boolean
  config_repo_owner?: string
  config_repo_name?: string
  config_repo_branch?: string
}

/** A `pb` request to connect GitHub, as the console reviews it (nothing is used up by reading it). */
export type GitHubLinkReview = {
  account_email: string
  machine_name: string
  requested_at: string
  expires_at: string
}

export const githubStatusQuery = queryOptions({
  queryKey: ["github-status"],
  queryFn: () => api<GitHubStatus>("github/status"),
  enabled: githubLive,
  staleTime: 30_000,
})

const json = { "content-type": "application/json" }
const linkPath = (id: string) => `github/native-links/${encodeURIComponent(id)}`

export const reviewLink = (id: string, token: string) =>
  api<GitHubLinkReview>(`${linkPath(id)}/review`, { method: "POST", headers: json, body: JSON.stringify({ token }) })

/** One use: returns GitHub's authorization page for this request. */
export const approveLink = (id: string, token: string) =>
  api<{ authorization_url: string }>(`${linkPath(id)}/approve`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ token }),
  })

export const cancelLink = (id: string, token: string) =>
  api<unknown>(`${linkPath(id)}/cancel`, { method: "POST", headers: json, body: JSON.stringify({ token }) })

/** Connect from the console: GitHub returns to /github/done, then back to `returnTo`. */
export const startConnect = (returnTo: string) =>
  api<{ authorization_url: string }>("github/oauth/start", {
    method: "POST",
    headers: json,
    body: JSON.stringify({ return_to: returnTo }),
  })

export const disconnectGitHub = () => api<GitHubStatus>("github/connection", { method: "DELETE" })

/** What went wrong, for /github/done?result=error&code=… and the review errors. */
export const githubErrors: Record<string, string> = {
  github_scope_denied: "GitHub didn’t grant the access Paperboat needs to read and write your config repository.",
  github_identity_conflict: "This GitHub account is already connected to another Paperboat account.",
  github_oauth_failed: "GitHub couldn’t confirm the connection.",
  github_link_expired: "This connection request expired.",
  github_link_used: "This connection request was already approved or cancelled.",
  github_link_other_account: "This request is for another Paperboat account. Sign in with the account that ran pb.",
  github_link_unavailable: "This link isn’t valid anymore; pb may have started a newer request.",
}
