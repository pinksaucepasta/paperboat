export const HEX_LIMIT = 4096

/** Bytes of a body the machine sent base64-labelled (`*_body_encoding`), else the text as is. */
export function bodyText(text: string, encoding?: string): { shown: string; note?: string } {
  if (encoding !== "base64") return { shown: text }
  const bytes = Uint8Array.from(atob(text), (c) => c.charCodeAt(0))
  const rows: string[] = []
  for (let i = 0; i < Math.min(bytes.length, HEX_LIMIT); i += 16) {
    const row = Array.from(bytes.subarray(i, i + 16))
    const hex = row.map((b) => b.toString(16).padStart(2, "0")).join(" ").padEnd(47)
    const ascii = row.map((b) => (b >= 32 && b < 127 ? String.fromCharCode(b) : ".")).join("")
    rows.push(`${i.toString(16).padStart(8, "0")}  ${hex}  ${ascii}`)
  }
  const cut = bytes.length > HEX_LIMIT ? `, first ${HEX_LIMIT} shown` : ""
  return { shown: rows.join("\n"), note: `Binary or non-UTF-8 body, ${bytes.length} bytes${cut}, as captured.` }
}
