/**
 * What the pricing page compares. Every feature is on every plan (PLAN 1.3); only the bandwidth
 * differs, and it comes from `plans` in site.ts. TODO-COPY marks the text still to be written.
 */
import { plans } from "./site"

/** Features, as people name them. All included on all plans. */
export const features = [
  "Terminals that survive a dropped connection, with replay",
  "Remote commands",
  "SSH, SCP, SFTP and rsync, with managed keys",
  "Send and receive files",
  "Previews of local ports",
  "Tunnels with permanent addresses and your own domains",
  "Environment variables, end-to-end encrypted",
  "Teams with roles and sharing",
  "Config sync from a Git repository",
  "The web console",
  "Self-hosted relays and tunnel servers",
  "Unlimited machines",
]

export const planNames = plans.map((plan) => plan.name)
export const bandwidthRow = plans.map((plan) => plan.bandwidth)

const todo = (what: string) => `TODO-COPY: ${what}`

/** “How bandwidth works”: short paragraphs, each to be written. */
export const bandwidthNotes = [
  { title: "What is always free", text: todo("direct machine-to-machine traffic costs nothing") },
  { title: "What uses your bandwidth", text: todo("traffic through our relays and edges, in plain words") },
  { title: "When you run out", text: todo("what happens and what to do") },
  { title: "Adding more", text: todo("how extra bandwidth works for one person and for a team") },
]

export const faq = [
  { question: todo("question 1"), answer: todo("answer 1") },
  { question: todo("question 2"), answer: todo("answer 2") },
  { question: todo("question 3"), answer: todo("answer 3") },
  { question: todo("question 4"), answer: todo("answer 4") },
  { question: todo("question 5"), answer: todo("answer 5") },
]
