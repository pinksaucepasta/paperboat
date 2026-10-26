/**
 * What the pricing page compares (user pricing decisions 2026-10-22). Free and Pro have everything
 * for one person and differ only in bandwidth, which comes from `plans` in site.ts; Team adds people.
 */
import { plans, teamTrial } from "./site"

const limitCells = (key: "machines" | "publications" | "browserTerminals" | "nativeTerminals"): [PlanCell, PlanCell, PlanCell] => [
  plans[0][key],
  plans[1][key],
  plans[2][key],
]

/** One row per feature: true is included, false is not, a string is shown as it is. */
export type PlanCell = boolean | string
export const featureRows: { feature: string; cells: [PlanCell, PlanCell, PlanCell] }[] = [
  { feature: "Machines", cells: limitCells("machines") },
  { feature: "Live previews and tunnels", cells: limitCells("publications") },
  { feature: "Live browser terminals", cells: limitCells("browserTerminals") },
  { feature: "Terminals from the pb command", cells: limitCells("nativeTerminals") },
  { feature: "Team members and shared machines", cells: [false, false, true] },
  { feature: "Self-hosted relays and tunnel servers", cells: [true, true, true] },
  { feature: "Invoices and receipts", cells: [false, true, true] },
]

export const planNames = plans.map((plan) => plan.name)
export const bandwidthRow = plans.map((plan) => plan.bandwidth)

/** “How bandwidth works”: short, plain answers. */
export const bandwidthNotes = [
  {
    title: "Direct traffic is always free",
    text: "When your machines connect directly, nothing is counted. That is most of the time.",
  },
  {
    title: "What counts",
    text: "Traffic through our relays, when machines can’t connect directly, and through our edges, for public previews and tunnels.",
  },
  {
    title: "When you run out",
    text: "Previews, tunnels and relayed connections pause until next month or until you add more. Direct connections keep working.",
  },
  {
    title: "Adding more",
    text: "Add 100 GB at a time from the console. On a team, it goes into one shared pool.",
  },
]

export const faq = [
  {
    question: "Is there a limit on machines?",
    answer: `Free allows ${plans[0].machines} machines and Pro ${plans[1].machines}. Team has no limit.`,
  },
  {
    question: "Are features locked behind a plan?",
    answer:
      "No. Free and Pro differ only in limits and bandwidth. Team adds members and lasting access to each other’s machines.",
  },
  {
    question: "What counts as a browser terminal?",
    answer: "A terminal open in the console. Terminals from the pb command are unlimited on every plan. A shared terminal counts once, with up to 50 viewers.",
  },
  {
    question: "Do I need Team to share with someone?",
    answer: "No. Sharing a terminal session or a preview link is free on every plan. Team is for people who work on each other’s machines daily.",
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
    answer: "You move back to Free. Your machines, terminals and settings stay. Bandwidth goes back to 5 GB.",
  },
]
