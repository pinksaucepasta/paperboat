"use client"

import { CircleNotchIcon, WarningCircleIcon } from "@phosphor-icons/react"
import { useMutation } from "@tanstack/react-query"
import Link from "next/link"
import { useRouter, useSearchParams } from "next/navigation"
import { useEffect, useRef, useState } from "react"
import { api, idempotencyKey, useApiQuery, machineQuery } from "@/lib/api"
import { Frame, TopBar } from "@/components/terminal/terminal-page"
import { Button } from "@/components/ui/button"

/**
 * `/terminals/new?machine=…&workspace=…`: where "New terminal" opens its tab. It starts the shell,
 * then replaces itself with the terminal, so the tab shows Paperboat the whole time.
 */
export function StartTerminal() {
  const router = useRouter()
  const params = useSearchParams()
  const machineId = params.get("machine") ?? ""
  const workspace = params.get("workspace") ?? "personal"
  const { data: machine } = useApiQuery({ ...machineQuery(machineId), enabled: Boolean(machineId) })
  // One key per page: a repeated request (React's development double-run, a retry) is the same shell.
  const [key, setKey] = useState(idempotencyKey)
  const start = useMutation({
    mutationFn: () =>
      api<{ id: string }>(
        `machines/${encodeURIComponent(machineId)}/terminal-sessions?workspace=${encodeURIComponent(workspace)}`,
        {
          method: "POST",
          headers: { "content-type": "application/json", "idempotency-key": key },
          body: JSON.stringify({}),
        },
      ),
    onSuccess: ({ id }) => router.replace(`/terminals/${encodeURIComponent(id)}`),
  })
  const started = useRef(false)
  useEffect(() => {
    if (!machineId || started.current) return
    started.current = true
    start.mutate()
  }, [machineId, start])

  const name = machine?.alias ?? "your machine"
  return (
    <Frame>
      <TopBar>
        <span className="text-sm text-muted-foreground">New terminal</span>
      </TopBar>
      <main className="flex flex-1 items-center justify-center p-8 text-center">
        {!machineId ? (
          <Problem title="No machine chosen" detail="Start a terminal from the Terminals page." />
        ) : start.isError ? (
          <Problem
            title={`Couldn’t start a shell on ${name}`}
            detail={start.error.message}
            retry={() => {
              setKey(idempotencyKey())
              start.reset()
              started.current = false
            }}
          />
        ) : (
          <p className="flex items-center gap-2 text-sm text-muted-foreground" role="status">
            <CircleNotchIcon className="size-4 animate-spin" />
            Starting a shell on {name}…
          </p>
        )}
      </main>
    </Frame>
  )
}

function Problem({ title, detail, retry }: { title: string; detail: string; retry?: () => void }) {
  return (
    <div className="flex max-w-sm flex-col items-center gap-2">
      <WarningCircleIcon className="size-6 text-muted-foreground" />
      <p className="font-medium">{title}</p>
      <p className="text-sm text-muted-foreground">{detail}</p>
      <div className="mt-2 flex gap-2">
        {retry && <Button onClick={retry}>Try again</Button>}
        <Button variant="secondary" nativeButton={false} render={<Link href="/terminals" />}>
          All terminals
        </Button>
      </div>
    </div>
  )
}
