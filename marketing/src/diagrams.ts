/** The connection paths, drawn the same way wherever they appear (home, security, pricing, features). */
import type { Lane } from "./components/Lanes.astro"

export const directLane: Lane = {
  name: "Direct",
  text: "Two machines that can reach each other talk peer to peer. This is the usual case, and it never counts against your plan.",
  nodes: [{ label: "laptop" }, { label: "studio" }],
  wires: ["e2e"],
}

export const relayLane: Lane = {
  name: "Relayed",
  text: "When a network blocks the direct path, a relay passes the same encrypted bytes along. It has no key to read them.",
  nodes: [{ label: "laptop" }, { label: "relay", kind: "ours" }, { label: "studio" }],
  wires: ["e2e", "e2e"],
}

export const publicLane: Lane = {
  name: "Public link",
  text: "A preview or tunnel is for people without Paperboat. Their HTTPS ends at the edge, which forwards the request to your machine.",
  nodes: [{ label: "any browser", kind: "public" }, { label: "edge", kind: "ours" }, { label: "studio:3000" }],
  wires: ["tls", "e2e"],
}
