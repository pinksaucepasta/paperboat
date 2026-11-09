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
  billing: (workspace: string) => ["billing", workspace],
  billingProducts: (workspace: string) => ["billing-products", workspace],
  inboxRequests: (workspace: string) => ["team-inbox", workspace],
  machineOverview: ["machines", "overview"],
  machine: (id: string) => ["machines", "one", id],
  machineSummary: (workspace: string) => ["machines", workspace, "summary"],
} as const
