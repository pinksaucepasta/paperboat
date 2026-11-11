"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, ApiError } from "@/lib/api"
import type { components } from "@/lib/api-schema"

// The team's own S3-compatible bucket (contract: shared/teambucket). The server stores the access key encrypted and
// never returns it; the team chooses what the bucket is for. Only ciphertext ever lands in it.

export type TeamBucket = components["schemas"]["TeamBucket"]
export type TeamBucketInput = components["schemas"]["TeamBucketInput"]

const path = (team: string) => `teams/${encodeURIComponent(team)}/bucket`

/** A team without a bucket answers 404 bucket_not_configured: that is `null` here, not an error. */
export const teamBucketQuery = (team: string) =>
  queryOptions({
    queryKey: ["team", team, "bucket"],
    queryFn: async ({ signal }) => {
      try {
        return await api<TeamBucket>(path(team), { signal })
      } catch (error) {
        if (error instanceof ApiError && error.code === "bucket_not_configured") return null
        throw error
      }
    },
    staleTime: 15_000,
  })

export const saveTeamBucket = (team: string, input: TeamBucketInput) =>
  api<TeamBucket>(path(team), {
    method: "PUT",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(input),
  })

export const removeTeamBucket = (team: string) => api<unknown>(path(team), { method: "DELETE" })

/** Server refusals worded for what the person can do next; a refused check carries the bucket's own words. */
export const bucketMessage = (error: unknown) =>
  error instanceof ApiError && error.code === "team_plan_required"
    ? "The team bucket is part of the Teams plan."
    : error instanceof Error
      ? error.message
      : "The team bucket is unavailable. Try again."
