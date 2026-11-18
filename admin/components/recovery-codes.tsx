"use client"

import { Button } from "@/components/ui/button"

/** Recovery codes, shown once: copy or print them now, they are not shown again. */
export function RecoveryCodes({ codes }: { codes: string[] }) {
  return (
    <div className="flex max-w-sm flex-col gap-2" data-recovery-codes>
      <p className="text-sm">Save these now. Each works once; they won’t be shown again.</p>
      <ul className="surface grid grid-cols-2 gap-1 p-3 font-mono text-xs">
        {codes.map((c) => (
          <li key={c}>{c}</li>
        ))}
      </ul>
      <Button variant="outline" size="lg" className="self-start" onClick={() => navigator.clipboard?.writeText(codes.join("\n")).catch(() => {})}>
        Copy all
      </Button>
    </div>
  )
}
