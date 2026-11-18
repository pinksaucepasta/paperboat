"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, apiAction, type Schemas } from "@/lib/api"
import { queryKeys } from "@/lib/query-keys"

export type Announcements = Schemas["Announcements"]
export type AnnouncementNotice = Schemas["AnnouncementNotice"]
export type AnnouncementBanner = Schemas["AnnouncementBanner"]

/**
 * What operators told this account: the ribbon above every page and the notices in the bell. It rides with the
 * session on the server, so a page never waits for it; the browser asks again only when the window regains
 * focus after five minutes. No announcement is an empty answer, and a failed read shows nothing.
 */
export const announcementsQuery = queryOptions({
  queryKey: queryKeys.announcements,
  queryFn: ({ signal }) => api<Announcements>("announcements", { signal }),
  staleTime: 5 * 60_000,
  retry: false,
})

const json = { "content-type": "application/json" }

export const markNoticesRead = (ids: string[]) =>
  apiAction("announcements/read", { method: "POST", headers: json, body: JSON.stringify({ ids }) })

export const dismissBanner = (id: string) => apiAction(`announcements/${encodeURIComponent(id)}/dismiss`, { method: "POST" })
