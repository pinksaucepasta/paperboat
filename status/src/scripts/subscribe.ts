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
