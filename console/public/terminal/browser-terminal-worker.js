import { TerminalOutputFlow } from "./output-flow.js"

const flows = new Map()
const connections = new Map()
let ended = false
function fatal() {
  if (ended) return
  ended = true
  for (const flow of flows.values()) flow.close()
  flows.clear()
  connections.clear()
  self.postMessage({ type: "fatal" })
  self.close()
}

async function start() {
  await import("./wasm_exec.js")
  const go = new globalThis.Go()
  const response = await fetch("./browserterminal.wasm", { cache: "no-cache" })
  if (!response.ok) throw new Error("runtime unavailable")
  const { instance } = await WebAssembly.instantiate(await response.arrayBuffer(), go.importObject)
  void go.run(instance).then(fatal, fatal)
  for (let attempt = 0; attempt < 200; attempt++) {
    if (globalThis.paperboatBrowserTerminal) return globalThis.paperboatBrowserTerminal
    await new Promise((resolve) => setTimeout(resolve, 10))
  }
  throw new Error("runtime unavailable")
}
const ready = start()
void ready.catch(fatal)

function leave(id, callbackID) {
  const key = callbackID ?? connections.get(id)
  if (key !== undefined) {
    flows.get(key)?.close()
    flows.delete(key)
  }
  connections.delete(id)
  globalThis.paperboatBrowserTerminal?.leave(id)
}

self.onmessage = async ({ data }) => {
  if (ended) return
  if (data.type === "consumed") { flows.get(data.callbackID)?.consumed(data.id); return }
  if (data.type === "leave") { leave(data.id, data.callbackID); return }
  if (data.type !== "call") return
  try {
    const api = await ready
    if (ended) return
    let value
    if (data.method === "ready") value = null
    else if (data.method === "connect") {
      if (flows.size >= 16) throw new Error("connection limit")
      const callbackID = data.callbackID
      let connectionID = ""
      const flow = new TerminalOutputFlow({
        send: (batch) => {
          const buffers = []
          for (const event of batch.events) {
            if (event.data) buffers.push(event.data.buffer)
            if (event.screen) buffers.push(event.screen.buffer)
          }
          self.postMessage({ type: "events", callbackID, batch }, [...new Set(buffers)])
        },
        overflow: () => {
          // A fresh authenticated checkpoint recovers a persistently slow
          // consumer. Never skip arbitrary VT bytes to pretend it caught up.
          self.postMessage({ type: "events", callbackID, batch: { id: -1, events: [{ type: "state", connection_id: connectionID, state: "reconnecting", code: "output_backpressure" }] } })
          // Leave outside the Go output callback; cleanup can invoke Go again.
          setTimeout(() => leave(connectionID, callbackID), 0)
        },
      })
      flows.set(callbackID, flow)
      value = api.connect(...data.args, (event) => {
        connectionID ||= event.connection_id
        flow.push(event)
      })
      if (value.id) {
        connectionID = value.id
        connections.set(connectionID, callbackID)
      } else {
        flow.close()
        flows.delete(callbackID)
      }
    } else if (["createIdentity", "inspectRoot", "discardIdentity", "sendInput", "resize", "ack", "cursor", "hashFile", "fileRequest"].includes(data.method)) {
      value = await api[data.method](...data.args)
    } else throw new Error("invalid method")
    self.postMessage({ type: "result", id: data.id, value })
  } catch {
    self.postMessage({ type: "result", id: data.id, error: true })
  }
}
