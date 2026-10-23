/**
 * What the pricing page compares (user pricing decisions 2026-10-22). Free and Pro have everything
 * for one person and differ only in bandwidth, which comes from `plans` in site.ts; Team adds people.
 */
import { plans, teamTrial } from "./site"

/** One row per feature: true is included, false is not, a string is shown as it is. */
export type PlanCell = boolean | string
export const featureRows: { feature: string; cells: [PlanCell, PlanCell, PlanCell] }[] = [
  { feature: "Machines", cells: ["Unlimited", "Unlimited", "Unlimited"] },
  { feature: "Terminals that survive a dropped connection, with replay", cells: [true, true, true] },
  { feature: "Remote commands", cells: [true, true, true] },
  { feature: "SSH, SCP, SFTP and rsync, with managed keys", cells: [true, true, true] },
  { feature: "Send and receive files", cells: [true, true, true] },
  { feature: "Previews and tunnels", cells: ["Unlimited", "Unlimited", "Unlimited"] },
  { feature: "Your own domains", cells: [true, true, true] },
  { feature: "Environment variables, end-to-end encrypted", cells: [true, true, true] },
  { feature: "Config sync from a Git repository", cells: [true, true, true] },
  { feature: "Tags and access rules", cells: [true, true, true] },
  { feature: "Share a terminal session or a preview link", cells: [true, true, true] },
  { feature: "Self-hosted relays and tunnel servers", cells: [true, true, true] },
  { feature: "Team members", cells: [false, false, true] },
  { feature: "Lasting access to each other’s machines", cells: [false, false, true] },
  { feature: "Roles", cells: [false, false, "Coming"] },
  { feature: "SSO and SCIM", cells: [false, false, "Coming"] },
  { feature: "Audit log export", cells: [false, false, "Coming"] },
  { feature: "Invoices and receipts", cells: [false, true, true] },
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
    answer:
      "Free and Pro have everything one person needs, with no limits on previews or tunnels; they differ only in bandwidth. Team adds people: members and lasting access to each other’s machines.",
  },
  {
    question: "Do I need Team to share with someone?",
    answer: "No. Sharing a terminal session or a preview link is free on every plan. Team is for people who work on each other’s machines every day.",
  },
  {
    question: "How does the Team trial work?",
    answer: teamTrial,
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
