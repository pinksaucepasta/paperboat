/**
 * What the pricing page compares (user pricing decisions 2026-10-22). Free and Pro have everything
 * for personal use and differ only in bandwidth, which comes from `plans` in site.ts; Team adds people.
 */
import { auditRetention, plans, publicTcp, ssoMinSeats, teamTrial, topUp } from "./site"

const limitCells = (key: "machines" | "publications" | "browserTerminals"): [PlanCell, PlanCell, PlanCell] => [
  plans[0][key],
  plans[1][key],
  plans[2][key],
]

/** One row per feature: true is included, false is not, a string is shown as it is. */
export type PlanCell = boolean | string
export const featureGroups: { title: string; rows: { feature: string; cells: [PlanCell, PlanCell, PlanCell] }[] }[] = [
  {
    title: "Limits",
    rows: [
      { feature: "Relay and edge bandwidth a month", cells: [plans[0].bandwidth, plans[1].bandwidth, plans[2].bandwidth] },
      { feature: "Direct traffic between your machines", cells: ["Unlimited", "Unlimited", "Unlimited"] },
      { feature: "Machines", cells: limitCells("machines") },
      { feature: "Previews and tunnels running at once", cells: limitCells("publications") },
      { feature: "Public raw-TCP tunnel addresses", cells: [publicTcp[0], publicTcp[1], publicTcp[2]] },
      { feature: "Terminals open in the web console at once", cells: limitCells("browserTerminals") },
      { feature: "Terminals from the pb command", cells: ["Unlimited", "Unlimited", "Unlimited"] },
      { feature: `Extra bandwidth, ${topUp.price} per ${topUp.size}`, cells: [false, true, true] },
    ],
  },
  {
    title: "On your machines",
    rows: [
      { feature: "Terminals that survive a dropped connection, with replay", cells: [true, true, true] },
      { feature: "Remote commands", cells: [true, true, true] },
      { feature: "SSH, SCP, SFTP and rsync with managed keys", cells: [true, true, true] },
      { feature: "File sending and the Inbox", cells: [true, true, true] },
      { feature: "Private access to ports", cells: [true, true, true] },
      { feature: "ENV: end-to-end encrypted environment variables", cells: [true, true, true] },
      { feature: "Config sync from a Git repository", cells: [true, true, true] },
    ],
  },
  {
    title: "Public links",
    rows: [
      { feature: "Previews and tunnels over HTTP/1.1, HTTP/2, HTTP/3 and WebSocket", cells: [true, true, true] },
      { feature: "Your own domains, with certificates", cells: [true, true, true] },
      { feature: "Request inspector and replay", cells: [true, true, true] },
      { feature: "Self-hosted relays and tunnel servers", cells: [true, true, true] },
    ],
  },
  {
    title: "Access and people",
    rows: [
      { feature: "Share a live terminal or a preview link", cells: [true, true, true] },
      { feature: "Tags, policy file and API tokens", cells: [true, true, true] },
      { feature: "Audit log kept for", cells: [auditRetention[0], auditRetention[1], auditRetention[2]] },
      { feature: "Team members with lasting access to shared machines", cells: [false, false, true] },
      { feature: "Roles: owner, admin, billing, auditor, member", cells: [false, false, true] },
      { feature: "Single sign-on (SAML, OIDC) and directory sync (SCIM)", cells: [false, false, `From ${ssoMinSeats} seats`] },
      { feature: "Audit log export and streaming", cells: [false, false, true] },
      { feature: "Terminal session recordings, archived to your own S3 bucket", cells: [false, false, true] },
      { feature: "Invoices and receipts", cells: [false, true, true] },
    ],
  },
]

export const planNames = plans.map((plan) => plan.name)

export const faq = [
  {
    q: "What exactly do I pay for?",
    a: "Bandwidth that passes through servers we run: relays, when two machines cannot connect directly, and edges, which serve public previews and tunnels. Traffic that goes straight between your machines is never counted, on any plan.",
  },
  {
    q: "What happens when the bandwidth runs out?",
    a: `Relayed connections, previews and tunnels pause until the next month, or until you add more on a paid plan (${topUp.price} per ${topUp.size}). Direct connections keep working, and you are told which limit was reached and where to raise it.`,
  },
  {
    q: "Are features locked behind a paid plan?",
    a: `No. Free and Pro have the same features and differ in limits and bandwidth. Team adds what a company needs: members, roles and audit log export, and single sign-on and directory sync for teams of ${ssoMinSeats} seats or more.`,
  },
  {
    q: "Do I need Team to share with someone?",
    a: "No. Sharing a live terminal or a preview link works on every plan. Team is for people who use each other's machines every day.",
  },
  {
    q: "What counts as a browser terminal?",
    a: "A terminal open in the web console. Terminals from the pb command are unlimited on every plan. A shared terminal counts once, however many of its 50 possible viewers are watching.",
  },
  { q: "How does the Team trial work?", a: teamTrial },
  {
    q: "What if I host my own relay or tunnel server?",
    a: 'Traffic through servers you run is not counted against your plan. <a href="/features/self-hosting/">Self-hosting</a> works on every plan, including Free.',
  },
  {
    q: "What if a payment fails?",
    a: "Paid benefits stay for 7 days while the payment is retried. After that the workspace drops to the free limits until the payment goes through. Your machines, sessions and settings are untouched.",
  },
  {
    q: "What happens to my machines if I cancel?",
    a: "You move back to Free. Machines, terminals and settings stay; only the limits change.",
  },
]
