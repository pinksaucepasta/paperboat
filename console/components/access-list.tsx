"use client"

import { XIcon } from "@phosphor-icons/react"
import { useRef, useState, type ReactNode } from "react"
import { cn } from "@/lib/utils"
import { isViewerEntry } from "@/lib/viewer-access"
import { Button } from "@/components/ui/button"

/**
 * The one list of who can open something in a browser besides its owner: email addresses and *@domain. Terminal
 * sharing, private previews and HTTP tunnel routes all use it. The server checks every entry again
 * (shared/viewerallow).
 *
 * `rowAction` adds a control to each entry (terminals: watch or type). `whenEmpty` shows in place of the empty list
 * (terminals: guests joining with a code); previews and tunnels have no guests and pass nothing.
 */
export function AccessList({
  entries,
  onChange,
  disabled,
  hint,
  rowAction,
  whenEmpty,
}: {
  entries: string[]
  onChange: (entries: string[]) => void
  disabled: boolean
  hint: string
  rowAction?: (entry: string) => ReactNode
  whenEmpty?: ReactNode
}) {
  return (
    <div className="flex min-w-0 flex-col gap-3">
      <EmailField
        disabled={disabled}
        onAdd={(added) => onChange([...entries, ...added.filter((entry) => !entries.includes(entry))])}
      />
      <p className="-mt-1 text-xs text-muted-foreground">{hint}</p>
      {entries.length > 0 ? (
        <ul className="-mx-2 flex max-h-56 flex-col overflow-y-auto">
          {entries.map((entry) => (
            <li key={entry} className="flex h-10 min-w-0 items-center gap-3 rounded-md px-2 text-sm">
              <span className="min-w-0 flex-1 truncate">
                {entry.startsWith("*@") ? `Everyone at ${entry.slice(2)}` : entry}
              </span>
              {rowAction?.(entry)}
              <Button
                size="icon-sm"
                variant="ghost"
                aria-label={`Remove ${entry}`}
                className="text-muted-foreground"
                disabled={disabled}
                onClick={() => onChange(entries.filter((other) => other !== entry))}
              >
                <XIcon />
              </Button>
            </li>
          ))}
        </ul>
      ) : (
        whenEmpty
      )}
    </div>
  )
}

/** Enter, comma, space or Tab adds what's typed (several at once when pasted); the list below shows them. */
function EmailField({ disabled, onAdd }: { disabled: boolean; onAdd: (emails: string[]) => void }) {
  const [draft, setDraft] = useState("")
  const [error, setError] = useState("")
  const inputRef = useRef<HTMLInputElement>(null)

  const commit = (text: string) => {
    const parts = text
      .split(/[\s,;]+/)
      .map((part) => part.trim().toLowerCase())
      .filter(Boolean)
    if (!parts.length) {
      setDraft("")
      return
    }
    const bad = parts.find((part) => !isViewerEntry(part))
    if (bad) {
      setDraft(text.trim())
      setError(`“${bad}” isn’t an email. Use name@company.com, or *@company.com for a whole domain.`)
      return
    }
    setError("")
    setDraft("")
    onAdd([...new Set(parts)])
  }

  return (
    <div className="flex flex-col gap-1">
      <div
        onClick={() => inputRef.current?.focus()}
        className={cn(
          "flex h-9 w-full cursor-text items-center rounded-md border border-input bg-transparent px-3 text-sm transition-colors focus-within:border-ring focus-within:ring-2 focus-within:ring-ring/30 dark:bg-input/30",
          error && "border-destructive",
        )}
      >
        <input
          ref={inputRef}
          value={draft}
          // Read-only, not disabled, while a change saves: the field keeps focus for the next email.
          readOnly={disabled}
          onChange={(event) => {
            const value = event.target.value
            // A separator typed ends an email.
            if (/[\s,;]$/.test(value)) commit(value)
            else {
              setDraft(value)
              if (error) setError("")
            }
          }}
          onKeyDown={(event) => {
            if ((event.key === "Enter" || event.key === "Tab") && draft.trim()) {
              event.preventDefault()
              commit(draft)
            }
          }}
          onBlur={() => draft.trim() && commit(draft)}
          onPaste={(event) => {
            const text = event.clipboardData.getData("text")
            if (/[\s,;]/.test(text.trim())) {
              event.preventDefault()
              commit(draft + text)
            }
          }}
          placeholder="Add people by email"
          aria-label="Add people by email or *@domain"
          autoComplete="off"
          spellCheck={false}
          className="h-6 min-w-32 flex-1 bg-transparent outline-none placeholder:text-muted-foreground"
        />
      </div>
      {error && <p className="text-xs text-destructive">{error}</p>}
    </div>
  )
}
