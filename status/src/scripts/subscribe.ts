// The subscribe dialog: a native <dialog> opened by any [data-subscribe-open] button; email posts to the same-origin API.
const dialog = document.getElementById("subscribe") as HTMLDialogElement
const form = document.getElementById("subscribe-form") as HTMLFormElement
const message = document.getElementById("subscribe-message") as HTMLElement
const feed = document.getElementById("subscribe-feed") as HTMLInputElement
feed.value = `${location.origin}/api/feed.xml`

document.querySelectorAll("[data-subscribe-open]").forEach((b) => b.addEventListener("click", () => { message.textContent = ""; dialog.showModal() }))
dialog.addEventListener("click", (e) => { if (e.target === dialog) dialog.close() }) // a click on the backdrop

document.getElementById("subscribe-copy")!.addEventListener("click", async (e) => {
  const button = e.currentTarget as HTMLButtonElement
  try { await navigator.clipboard.writeText(feed.value); button.textContent = "Copied" } catch { feed.select(); button.textContent = "Press Ctrl+C" }
  setTimeout(() => (button.textContent = "Copy"), 2000)
})

form.addEventListener("submit", async (e) => {
  e.preventDefault()
  const email = (form.elements.namedItem("email") as HTMLInputElement).value.trim()
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) { message.textContent = "Enter a valid email address."; return }
  const button = form.querySelector("button[type=submit]") as HTMLButtonElement
  button.disabled = true
  try {
    const res = await fetch("/api/subscribe", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ email }) })
    message.textContent = res.ok ? "Check your inbox to confirm." : ((await res.json().catch(() => ({}))) as { error?: string }).error ?? "Could not subscribe. Try again."
  } catch { message.textContent = "Could not subscribe. Check your connection and try again." }
  button.disabled = false
})

// Webhook: the Worker checks the address answers a challenge before it stores anything, then returns the signing secret once.
const hookForm = document.getElementById("webhook-form") as HTMLFormElement
const hookMessage = document.getElementById("webhook-message") as HTMLElement
hookForm.addEventListener("submit", async (e) => {
  e.preventDefault()
  const url = (hookForm.elements.namedItem("url") as HTMLInputElement).value.trim()
  const button = hookForm.querySelector("button[type=submit]") as HTMLButtonElement
  button.disabled = true
  hookMessage.textContent = "Checking the address…"
  try {
    const res = await fetch("/api/webhook/subscribe", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ url }) })
    const data = (await res.json().catch(() => ({}))) as { error?: string; secret?: string; unsubscribe_token?: string }
    if (res.ok && data.secret) {
      hookMessage.textContent = "Subscribed."
      ;(document.getElementById("webhook-secret-value") as HTMLInputElement).value = data.secret
      ;(document.getElementById("webhook-token-value") as HTMLInputElement).value = data.unsubscribe_token ?? ""
      ;(document.getElementById("webhook-secret") as HTMLElement).hidden = false
    } else hookMessage.textContent = data.error ?? "Could not subscribe. Try again."
  } catch { hookMessage.textContent = "Could not subscribe. Check your connection and try again." }
  button.disabled = false
})
