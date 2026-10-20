/**
 * The compare pages: one entry per product, one template (pages/compare/[slug].astro). Claims about
 * other products must match their own documentation; recheck before each release of the site.
 */
export type CompareRow = { feature: string; them: string; us: string }

export type Compare = {
  slug: string
  name: string
  /** The page title and the one line under it. */
  title: string
  summary: string
  rows: CompareRow[]
  themWhen: string[]
  usWhen: string[]
  switching: string[]
}

/** What Paperboat does, the same on every page. */
const us = [
  "Terminals that keep running on the machine, with replay; from any terminal, browser or phone",
  "pb ssh or plain ssh, plus SCP, SFTP and rsync; keys handled for you",
  "pb preview gives a port a public link in one command",
  "Tunnels with a permanent address and your own domains",
  "Machines reach each other’s ports privately on 127.100.x.x addresses",
  "Yes, between your machines; public links end TLS at our edge",
  "Teams, roles, and sharing per machine, terminal or preview",
  "Run your own relays and tunnel servers",
  "Free to start; every feature on every plan; you pay for bandwidth",
]

/** The rows every comparison answers, in this order. */
const rowNames = [
  "Remote terminal",
  "SSH and file copy",
  "Public links for local ports",
  "Permanent addresses and your own domains",
  "Private network between machines",
  "End-to-end encryption",
  "Teams, roles and sharing",
  "Self-hosting",
  "Pricing",
]

function entry(
  slug: string,
  name: string,
  summary: string,
  them: string[],
  themWhen: string[],
  usWhen: string[],
  switching: string[],
): Compare {
  return {
    slug,
    name,
    title: `Paperboat vs ${name}`,
    summary,
    rows: rowNames.map((feature, index) => ({ feature, them: them[index], us: us[index] })),
    themWhen,
    usWhen,
    switching,
  }
}

const switchSteps = (from: string) => [
  "Install Paperboat on each machine with the one-line installer.",
  `Move one thing at a time (a terminal, a preview, a tunnel) and keep ${from} running until it works.`,
  `Remove ${from} from a machine once nothing depends on it.`,
]

export const compares: Compare[] = [
  entry(
    "tailscale",
    "Tailscale",
    "Tailscale builds a private network between your devices. Paperboat is built around working on your machines: terminals, previews, tunnels and files.",
    [
      "SSH through Tailscale SSH; no terminal that survives a dropped connection",
      "Regular SSH tools over the private network",
      "Funnel shares a port publicly on a ts.net address",
      "No custom domains for Funnel",
      "Yes, a full mesh VPN between devices",
      "Yes (WireGuard)",
      "Access rules (ACLs), users and groups",
      "Only through Headscale, a separate community project",
      "Free personal plan; paid plans per user",
    ],
    ["You want a full VPN: every device and subnet on one private network.", "You need detailed network access rules for a whole company."],
    ["You mostly want to work on your machines: terminals, SSH, previews and files.", "You want public links and permanent tunnels in the same tool."],
    switchSteps("Tailscale"),
  ),
  entry(
    "ngrok",
    "ngrok",
    "ngrok gives a local port a public URL. Paperboat does that too, and also gives you terminals, SSH and files on the same machines.",
    [
      "None",
      "None",
      "Yes, its core feature",
      "Yes, on paid plans",
      "None",
      "Traffic ends TLS at ngrok’s edge",
      "Team accounts on paid plans",
      "No",
      "Free tier with limits; paid plans",
    ],
    ["You only need public URLs, with traffic features like request inspection and policies at scale.", "You need its integrations and API gateway features."],
    ["You also want to reach and work on the machine itself, not just share a port.", "You want previews and permanent tunnels with one install."],
    switchSteps("ngrok"),
  ),
  entry(
    "cloudflare-tunnel",
    "Cloudflare Tunnel",
    "Cloudflare Tunnel puts services behind Cloudflare’s network. Paperboat gives you tunnels plus terminals, SSH, previews and files, without moving your domain.",
    [
      "Browser SSH through Cloudflare Access",
      "SSH through cloudflared and Access",
      "Quick tunnels on a random trycloudflare.com address",
      "Yes, on a domain that uses Cloudflare DNS",
      "Yes, with the WARP client",
      "Traffic ends TLS at Cloudflare",
      "Cloudflare Zero Trust users and policies",
      "No",
      "Tunnels are free; Zero Trust has paid plans",
    ],
    ["Your domain is already on Cloudflare and you want its security and caching in front.", "You need Cloudflare’s Zero Trust policies across a company."],
    ["You don’t want to move your DNS to Cloudflare.", "You want terminals, previews and files on the same machines, not only tunnels."],
    switchSteps("cloudflared"),
  ),
  entry(
    "teleport",
    "Teleport",
    "Teleport is an access platform for companies: SSH, Kubernetes, databases and apps with audit. Paperboat is lighter: install it and you’re working in a minute.",
    [
      "Yes, with session recording",
      "Yes, certificate-based SSH",
      "App access to internal web apps, not public links",
      "Not for public sharing",
      "Not a mesh network",
      "TLS to its proxy; access via short-lived certificates",
      "Detailed roles, access requests and audit",
      "Yes",
      "Free Community Edition; paid Enterprise",
    ],
    ["You need audit, compliance and access requests across many servers.", "You manage Kubernetes clusters and databases for a company."],
    ["You want fast access to your own machines without running an access platform.", "You want public previews and tunnels too."],
    switchSteps("Teleport"),
  ),
  entry(
    "netbird",
    "NetBird",
    "NetBird is an open-source private network between devices. Paperboat focuses on working on your machines: terminals, previews, tunnels and files.",
    [
      "SSH through NetBird SSH; no terminal that survives a dropped connection",
      "Regular SSH tools over the private network",
      "None",
      "None",
      "Yes, a mesh VPN between devices",
      "Yes (WireGuard)",
      "Access policies, users and groups",
      "Yes, you can run the whole service yourself",
      "Free tier; paid plans per user",
    ],
    ["You want a fully open-source VPN you can host end to end.", "You need network access rules across a company."],
    ["You want terminals, previews, public links and file sending in one tool.", "You’d rather not run a network control server yourself."],
    switchSteps("NetBird"),
  ),
]
