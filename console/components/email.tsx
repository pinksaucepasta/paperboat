"use client"

import { useMemo, useState } from "react"
import { useHideEmail } from "@/lib/privacy"
import { cn } from "@/lib/utils"

/**
 * An email address that honours "Hide my email": random letters of the same shape under a soft
 * blur. The blur reads as "hidden"; the letters mean removing it still reveals nothing.
 * Where `revealable`, clicking it shows the real address until the page is left.
 */
export function Email({
  value,
  revealable = false,
  className,
}: {
  value: string
  revealable?: boolean
  className?: string
}) {
  const hidden = useHideEmail()
  const [revealed, setRevealed] = useState(false)
  const standIn = useMemo(() => scramble(value), [value])
  if (!hidden || revealed) return <span className={className}>{value}</span>

  // The blur sits on an inner span: WebKit paints a coloured edge when a filter meets the
  // overflow clipping that `truncate` adds.
  // Monospace, so random letters take the same room whichever they are.
  const blurred = <span className="font-mono text-[0.9em] blur-[3px] group-hover/email:blur-[2px]">{standIn}</span>
  if (!revealable) return <span className={cn("select-none", className)}>{blurred}</span>
  return (
    <button
      type="button"
      title="Show email"
      className={cn(
        "group/email cursor-pointer rounded-sm text-left outline-none select-none focus-visible:ring-2 focus-visible:ring-ring/30",
        className,
      )}
      onClick={() => setRevealed(true)}
    >
      {blurred}
    </button>
  )
}

const letters = "abcdefghijklmnopqrstuvwxyz"

function scramble(email: string): string {
  const random = new Uint8Array(email.length)
  crypto.getRandomValues(random)
  return Array.from(email, (char, index) =>
    /[a-z0-9]/i.test(char) ? letters[random[index] % letters.length] : char,
  ).join("")
}
