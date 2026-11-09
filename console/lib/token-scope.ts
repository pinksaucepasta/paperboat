import { actionLabels, machineActions, type ScopeEntry } from "@/lib/policy"

/** What a token's permission rows say, in the order the console lists them. */
export const permissionDetails: Record<(typeof machineActions)[number], string> = {
  terminal: "Open terminals",
  exec: "Run commands with pb exec",
  managed_ssh: "Connect with SSH, SCP and SFTP",
  files: "Send and receive files",
  preview_manage: "Create and manage previews",
  tunnel_manage: "Create and manage tunnels",
  private_access: "Reach machine ports privately",
  inspect: "Inspect requests through previews and tunnels",
  replay: "Replay captured requests",
}

export const expiryChoices = [
  { id: "7", days: 7, label: "7 days" },
  { id: "30", days: 30, label: "30 days" },
  { id: "90", days: 90, label: "90 days" },
  { id: "365", days: 365, label: "1 year" },
  { id: "custom", days: 0, label: "Custom date" },
] as const

const day = 24 * 60 * 60 * 1000

/** The whole days from now to `date` (YYYY-MM-DD, the viewer's day), at least 1: the token then ends on that date. */
export function daysUntil(date: string, now: number): number {
  const [year, month, dayOfMonth] = date.split("-").map(Number)
  const end = new Date(year, month - 1, dayOfMonth, 23, 59, 59).getTime()
  return Math.max(1, Math.floor((end - now) / day))
}

/** `YYYY-MM-DD` of `now` plus `days`, in the viewer's zone, for a date input's bounds. */
export function dateInDays(days: number, now: number): string {
  const date = new Date(now + days * day)
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

/** The scope the API takes: one entry with the chosen machines (`machine:*` for all) and actions. */
export function tokenScope(machines: string[] | "all", actions: string[]): ScopeEntry[] {
  return [{ dst: machines === "all" ? ["machine:*"] : machines, allow: machineActions.filter((a) => actions.includes(a)) }]
}

/** A target in words: `tag:prod` stays, `machine:hp` → hp. */
export const targetName = (target: string) => target.replace(/^machine:/, "")

/** Allowed actions in plain words, e.g. "Terminal, Run commands". */
export const actionNames = (actions: string[]) =>
  machineActions
    .filter((a) => actions.includes(a))
    .map((a) => actionLabels[a])
    .join(", ")
