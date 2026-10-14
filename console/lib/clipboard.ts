"use client"

import { toast } from "sonner"

/**
 * Copies text, and says whether it worked. Browsers may refuse the async Clipboard API (Safari
 * does when focus has just moved, such as a menu closing, and anywhere outside HTTPS), so this
 * falls back to the older copy command on a temporary text field. Every copy in the console goes
 * through here, so a refusal never surfaces as an unhandled error.
 */
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    const field = document.createElement("textarea")
    field.value = text
    field.setAttribute("readonly", "")
    field.style.position = "fixed"
    field.style.opacity = "0"
    document.body.append(field)
    field.select()
    try {
      return document.execCommand("copy")
    } catch {
      return false
    } finally {
      field.remove()
    }
  }
}

/** Copies and confirms with a toast, or says it couldn't (`what` names the thing: "Address"). */
export async function copyWithToast(text: string, what: string, description?: string) {
  if (await copyText(text)) toast.success(`${what} copied`, { description })
  else toast.error(`Couldn’t copy the ${what.toLowerCase()}`, { description: "Select it and copy it yourself." })
}
