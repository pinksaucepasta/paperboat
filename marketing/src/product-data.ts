/**
 * The product pages: one entry per product, one template (pages/product/[slug].astro). Every text
 * marked TODO-COPY is a placeholder until the copy is written. The command examples are real: the
 * commands and the output lines come from the CLI's own code, and the addresses from the server's
 * client configuration (`{previewDomain}`, `{tunnelDomain}`, `{localDomain}`, `{selfhostInstallUrl}`
 * are filled in when the site builds). A line starting with "$ " is a command you type; where the
 * command opens a terminal and prints nothing of its own, only the commands are shown.
 */
export type Product = {
  slug: string
  /** The name in the navigation and in titles. */
  name: string
  /** The page's one headline, and the line under it. */
  headline: string
  summary: string
  /** Exactly three short blocks. */
  benefits: { title: string; text: string }[]
  example: { title: string; text: string }
  /** Exactly three steps. */
  steps: { title: string; text: string }[]
  /** Compare pages that apply, by slug (see compare-data.ts). */
  compare: string[]
}

const todo = (what: string) => `TODO-COPY: ${what}`

function placeholders(name: string) {
  return {
    headline: todo(`one plain line for ${name}`),
    summary: todo(`one sentence under the headline for ${name}`),
    benefits: [1, 2, 3].map((n) => ({ title: todo(`benefit ${n} title`), text: todo(`benefit ${n}, one or two sentences`) })),
    steps: [1, 2, 3].map((n) => ({ title: todo(`step ${n} title`), text: todo(`step ${n}, one sentence`) })),
  }
}

export const products: Product[] = [
  {
    slug: "terminals",
    name: "Terminals",
    ...placeholders("terminals"),
    example: {
      title: "Open a terminal on a machine",
      text: ["$ pb new", "$ pb connect studio"].join("\n"),
    },
    compare: ["tailscale", "teleport"],
  },
  {
    slug: "ssh",
    name: "SSH",
    ...placeholders("SSH"),
    example: {
      title: "Plain SSH tools, with keys handled for you",
      text: ["$ pb ssh studio", "$ ssh studio.{localDomain}", "$ rsync -a ./site studio.{localDomain}:/srv"].join("\n"),
    },
    compare: ["tailscale", "teleport"],
  },
  {
    slug: "previews",
    name: "Previews",
    ...placeholders("previews"),
    example: {
      title: "Share a local port",
      text: [
        "$ pb preview 3000",
        "Preview ready",
        "https://quiet-river-lantern-4821.{previewDomain} -> http://127.0.0.1:3000",
        "",
        "Press Ctrl+C to stop",
      ].join("\n"),
    },
    compare: ["ngrok", "cloudflare-tunnel"],
  },
  {
    slug: "tunnels",
    name: "Tunnels",
    ...placeholders("tunnels"),
    example: {
      title: "A permanent address for a service",
      text: [
        "$ pb tunnel create api --port 8080",
        "Created tunnel api (tunnel_4kq81zc2)",
        "Connector connector_9d2f7a is ready",
        "https://calm-summer-beach-3476.{tunnelDomain} -> http://127.0.0.1:8080",
        "Status: ready",
      ].join("\n"),
    },
    compare: ["ngrok", "cloudflare-tunnel"],
  },
  {
    slug: "private-access",
    name: "Private access",
    ...placeholders("private access"),
    example: {
      title: "Reach a port on a machine without publishing it",
      text: "$ pb access machine studio --port 5432",
    },
    compare: ["tailscale", "netbird"],
  },
  {
    slug: "files",
    name: "Files",
    ...placeholders("files"),
    example: {
      title: "Send a file to another machine",
      text: [
        "$ pb send ./report.pdf --to studio",
        "Sending to studio (machine_8a1d5c2e-3b47-4f0e-9d62-1c7e5b0a4f93)",
        "report.pdf: delivered to /home/ana/Paperboat/Inbox/report.pdf on studio",
      ].join("\n"),
    },
    compare: ["tailscale"],
  },
  {
    slug: "env",
    name: "Environment variables",
    ...placeholders("environment variables"),
    example: {
      title: "Store a secret, encrypted before it leaves your machine",
      text: ["$ pb env set API_KEY", "$ pb env list"].join("\n"),
    },
    compare: [],
  },
  {
    slug: "config-sync",
    name: "Config sync",
    ...placeholders("config sync"),
    example: {
      title: "Keep dotfiles the same on every machine",
      text: ["$ pb config status", "$ pb config sync"].join("\n"),
    },
    compare: [],
  },
  {
    slug: "teams",
    name: "Teams",
    ...placeholders("teams"),
    example: {
      title: "Invite someone and share a machine",
      text: ["$ pb team invite my-team ana@example.com", "$ pb team machine grant my-team machine_8a1d5c2e-3b47-4f0e-9d62-1c7e5b0a4f93"].join("\n"),
    },
    compare: ["tailscale", "teleport", "netbird"],
  },
  {
    slug: "self-hosting",
    name: "Self-hosting",
    ...placeholders("self-hosting"),
    example: {
      title: "Run your own relay or tunnel server",
      text: "$ curl -fsSL {selfhostInstallUrl} | sh",
    },
    compare: ["netbird", "cloudflare-tunnel"],
  },
]
