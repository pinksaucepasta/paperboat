/**
 * Query keys shared by browser queries (`lib/api.ts`) and the server prefetch
 * (`lib/server/prefetch.ts`), so data fetched on the server lands where the browser looks for it.
 */
export const queryKeys = {
  me: ["me"],
  workspaces: ["workspaces"],
  machines: (workspace: string) => ["machines", workspace],
  /** One page of the Machines table: search text, filters, page number. */
  machineList: (workspace: string, text: string, filters: object, page: number) =>
    ["machines", workspace, "list", text, filters, page] as const,
  usage: (workspace: string, period: string) => ["usage", period, workspace],
} as const
