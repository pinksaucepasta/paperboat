import { Mono, StatusBadge } from "@/components/filters"

/** An account's or team's state, as "Banned" with the operator's reference while a ban runs. */
export function BanState({ banned, state, reason }: { banned: boolean; state: string; reason?: string }) {
  return (
    <span className="inline-flex flex-wrap items-center gap-x-2">
      <StatusBadge state={banned ? "banned" : state} />
      {banned && reason && <Mono>{reason}</Mono>}
    </span>
  )
}
