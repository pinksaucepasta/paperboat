/**
 * What the pricing page compares. Every feature is on every plan (PLAN 1.3); only the bandwidth
 * differs, and it comes from `plans` in site.ts.
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

/** “How bandwidth works”: short, plain answers. */
export const bandwidthNotes = [
  {
    title: "Direct traffic is always free",
    text: "When your machines can reach each other directly, nothing is counted. That is most of the time.",
  },
  {
    title: "What counts",
    text: "Traffic that goes through our relays (when machines can’t connect directly) and through our edges (public previews and tunnels).",
  },
  {
    title: "When you run out",
    text: "Previews, tunnels and relayed connections pause until next month or until you add more. Direct connections keep working.",
  },
  {
    title: "Adding more",
    text: "Add 100 GB at a time from the console, whenever you need it. On a team, it goes into one shared pool.",
  },
]

export const faq = [
  {
    question: "Is there a limit on machines?",
    answer: "No. Every plan has unlimited machines.",
  },
  {
    question: "Are features locked behind a plan?",
    answer: "No. Every plan has every feature. Plans only differ in bandwidth.",
  },
  {
    question: "What if I host my own relay or tunnel server?",
    answer: "Traffic through servers you host yourself isn’t counted against your plan.",
  },
  {
    question: "Can I pay yearly?",
    answer: "Yes. Pro is $80 a year and Team is $150 per seat a year.",
  },
  {
    question: "What happens to my machines if I cancel?",
    answer: "You move back to Free. Your machines, terminals and settings stay; only the bandwidth goes back to 5 GB.",
  },
]
