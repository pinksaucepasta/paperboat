/**
 * Query keys shared by browser queries (`lib/api.ts`) and the server prefetch
 * (`lib/server/prefetch.ts`), so data fetched on the server lands where the browser looks for it.
 */
export const queryKeys = {
  me: ["me"],
  workspaces: ["workspaces"],
  machines: (workspace: string) => ["machines", workspace],
  usage: (workspace: string, period: string) => ["usage", period, workspace],
} as const
