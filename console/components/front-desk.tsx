"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { useApiQuery, type Machine } from "@/lib/api"
import { frontDeskQuery, saveFrontDesk, validPort, type FrontDesk as Desk } from "@/lib/port-names"
import { Choice } from "@/components/choice"
import { useCurrentMachine, useMachineLoading } from "@/components/machine-shell"
import { SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ErrorMessage } from "@/components/error-reference"

const schemes = [
  { value: "http", label: "http" },
  { value: "https", label: "https" },
]

/**
 * Where a name that matches no port name goes on this machine (a reverse proxy such as Traefik or
 * Caddy). Those who manage the machine change it; others see it.
 */
export function FrontDesk() {
  const machine = useCurrentMachine()
  const loading = useMachineLoading()
  const desk = useApiQuery({ ...frontDeskQuery(machine.id), enabled: !loading })
  return (
    <section className="flex flex-col gap-3">
      <SectionHeading title="Front desk" />
      <div className="flex min-h-11 flex-wrap items-center gap-x-4 gap-y-2 border-y border-divider py-1 text-sm">
        <span className="min-w-0 flex-1 text-muted-foreground">Addresses that match no name go here.</span>
        {desk.isError ? (
          <span className="text-destructive">Couldn’t load the front desk.</span>
        ) : desk.data ? (
          <Editor key={`${desk.data.port}-${desk.data.scheme}`} machine={machine} current={desk.data} />
        ) : (
          <Skeleton className="my-1 h-8 w-48" />
        )}
      </div>
    </section>
  )
}

function Editor({ machine, current }: { machine: Machine; current: Desk }) {
  const queryClient = useQueryClient()
  const [portText, setPortText] = useState(String(current.port))
  const [scheme, setScheme] = useState(current.scheme)
  const port = Number(portText)
  const dirty = port !== current.port || scheme !== current.scheme
  const save = useMutation({
    mutationFn: () => saveFrontDesk(machine.id, { port, scheme }),
    onSuccess: async (saved) => {
      queryClient.setQueryData(frontDeskQuery(machine.id).queryKey, saved)
      toast.success(`Front desk set to ${saved.scheme} on ${saved.port}`)
    },
  })
  if (!machine.can_manage)
    return (
      <span className="font-mono text-code">
        {current.scheme} on {current.port}
      </span>
    )
  return (
    <form
      className="flex items-center gap-2"
      onSubmit={(event) => {
        event.preventDefault()
        if (dirty && validPort(port)) save.mutate()
      }}
    >
      <Choice label={scheme} value={scheme} options={schemes} onChange={(next) => setScheme(next as Desk["scheme"])} />
      <span className="text-muted-foreground">on</span>
      <Input
        aria-label="Front desk port"
        inputMode="numeric"
        value={portText}
        onChange={(event) => setPortText(event.target.value)}
        aria-invalid={!validPort(port)}
        className="h-8 w-20 font-mono"
      />
      {(dirty || save.isPending) && (
        <Button type="submit" disabled={!validPort(port) || save.isPending}>
          {save.isPending ? "Saving…" : "Save"}
        </Button>
      )}
      {save.isError && (
        <span className="text-destructive">
          <ErrorMessage error={save.error} />
        </span>
      )}
    </form>
  )
}
