"use client"

import { Button } from "@/components/ui/button"

/** Recovery codes, shown once: copy or print them now, they are not shown again. */
export function RecoveryCodes({ codes }: { codes: string[] }) {
  return (
    <div className="flex flex-col gap-2" data-recovery-codes>
      <p className="text-sm">
        Save these recovery codes somewhere safe. Each works once if you lose your authenticator. They are not shown again.
      </p>
      <ul className="grid grid-cols-2 gap-1 rounded-md border p-3 font-mono text-xs">
        {codes.map((c) => (
          <li key={c}>{c}</li>
        ))}
      </ul>
      <Button variant="outline" size="sm" onClick={() => navigator.clipboard?.writeText(codes.join("\n")).catch(() => {})}>
        Copy all
      </Button>
    </div>
  )
}
