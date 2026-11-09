/**
 * Query keys shared by browser queries (`lib/api.ts`) and the server prefetch
 * (`lib/server/prefetch.ts`), so data fetched on the server lands where the browser looks for it.
 */
export const queryKeys = {
  me: ["me"],
  clientConfig: ["client-configuration"],
  workspaces: ["workspaces"],
  machines: (workspace: string) => ["machines", workspace],
  /** One page of the Machines table: search text, filters, page number. */
  machineList: (workspace: string, text: string, filters: object, page: number) =>
    ["machines", workspace, "list", text, filters, page] as const,
  usage: (workspace: string, period: string) => ["usage", period, workspace],
  billing: (workspace: string) => ["billing", workspace],
  billingProducts: (workspace: string) => ["billing-products", workspace],
  inboxRequests: (workspace: string) => ["team-inbox", workspace],
  apiTokens: (workspace: string) => ["api-tokens", workspace],
  tags: (workspace: string) => ["policy", workspace, "tags"],
  githubStatus: ["github-status"],
  /** A config-sync status page; `options` as configStatusQuery takes them (lib/config-sync.ts). */
  configStatus: (options: object) => ["config-sync", options],
  /** `params`: the activity filter as its query string (lib/activity.ts activityParams). */
  activity: (params: string) => ["activity", params],
  machineOverview: ["machines", "overview"],
  machine: (id: string) => ["machines", "one", id],
  machineSummary: (workspace: string) => ["machines", workspace, "summary"],
} as const

/**
 * placeholderData for a workspace's paged lists: keeps the last page on screen while paging or
 * searching, but never across workspaces (the workspace is each such key's second part), so a
 * switch never shows the other workspace's rows.
 */
export const keepWithinWorkspace =
  (workspace: string) =>
  <T>(previous: T | undefined, previousQuery?: { queryKey: readonly unknown[] }) =>
    previousQuery?.queryKey[1] === workspace ? previous : undefined
