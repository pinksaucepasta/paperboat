/**
 * The product pages: one entry per product, one template (pages/product/[slug].astro).  The command examples are real: the
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

export const products: Product[] = [
  {
    slug: "terminals",
    name: "Terminals",
    headline: "Terminals that don’t die.",
    summary: "Your shell runs on the machine, not in your laptop’s connection.",
    benefits: [
      { title: "Survives drops", text: "Close the lid, lose Wi-Fi, switch networks. The shell keeps running and you pick up where you left off." },
      { title: "See what you missed", text: "Output that ran while you were away is replayed when you come back." },
      { title: "Anywhere", text: "Open the same terminal from your terminal app, the browser or your phone." },
    ],
    steps: [
      { title: "Install", text: "Run the one-line installer on the machine." },
      { title: "Open", text: "Run pb new, or pick the machine in the console." },
      { title: "Come back", text: "Run pb connect from anywhere to get the same terminal back." },
    ],
    example: {
      title: "Open a terminal on a machine",
      text: ["$ pb new", "$ pb connect studio"].join("\n"),
    },
    compare: ["tailscale", "teleport"],
  },
  {
    slug: "ssh",
    name: "SSH",
    headline: "Plain SSH, without the key chores.",
    summary: "Use pb ssh, or your usual ssh, scp and rsync.",
    benefits: [
      { title: "Works with your tools", text: "VS Code, Cursor, Zed, scp, rsync and anything else that speaks SSH." },
      { title: "Keys handled", text: "Paperboat sets up and rotates the keys. No authorized_keys editing." },
      { title: "No open ports", text: "Machines don’t need a public IP or an open SSH port." },
    ],
    steps: [
      { title: "Install", text: "Run the installer on each machine." },
      { title: "Connect", text: "Run pb ssh homelab." },
      { title: "Use any tool", text: "Point ssh, scp or rsync at homelab.pprbt." },
    ],
    example: {
      title: "Plain SSH tools, with keys handled for you",
      text: ["$ pb ssh studio", "$ ssh studio.pprbt", "$ rsync -a ./site studio.pprbt:/srv"].join("\n"),
    },
    compare: ["tailscale", "teleport"],
  },
  {
    slug: "previews",
    name: "Previews",
    headline: "Share a port in one command.",
    summary: "pb preview gives the app on your machine a public link.",
    benefits: [
      { title: "Instant", text: "The link is ready in seconds. Nothing to deploy." },
      { title: "You choose who sees it", text: "Open to anyone, only you, or your team." },
      { title: "Gone when you stop", text: "Press Ctrl+C and the link stops working." },
    ],
    steps: [
      { title: "Start your app", text: "Run it on any port, like 3000." },
      { title: "Run pb preview 3000", text: "You get a link right away." },
      { title: "Share it", text: "Send the link. Stop it when you’re done." },
    ],
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
    headline: "A permanent address for a service.",
    summary: "Tunnels keep the same address, survive restarts, and can use your own domain.",
    benefits: [
      { title: "Stays put", text: "The address doesn’t change when the machine or the app restarts." },
      { title: "Your domain", text: "Add your own domain; certificates are handled for you." },
      { title: "Many routes", text: "Send different paths or ports on one tunnel to different apps." },
    ],
    steps: [
      { title: "Create", text: "Run pb tunnel create api --port 8080." },
      { title: "Point a domain", text: "Optionally add your domain and follow the DNS steps." },
      { title: "Leave it running", text: "It keeps serving, through restarts and updates." },
    ],
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
    headline: "Reach any port on your machines, privately.",
    summary: "Every machine gets a local address only your other machines can reach.",
    benefits: [
      { title: "Any protocol", text: "Databases, Redis, internal web apps: anything on TCP." },
      { title: "Encrypted end to end", text: "Only your machines can read the traffic." },
      { title: "Nothing exposed", text: "No public ports, no firewall changes." },
    ],
    steps: [
      { title: "Install on both machines", text: "Run the installer on each one." },
      { title: "Find the address", text: "Run pb machine list to see each machine’s 127.100.x.x address." },
      { title: "Connect", text: "Point your app at that address and port." },
    ],
    example: {
      title: "Reach a port on a machine without publishing it",
      text: "$ pb access machine studio --port 5432",
    },
    compare: ["tailscale", "netbird"],
  },
  {
    slug: "files",
    name: "Files",
    headline: "Send files between your machines.",
    summary: "pb send puts a file in another machine’s Inbox.",
    benefits: [
      { title: "Straight to the machine", text: "Files go machine to machine, encrypted. We don’t store them." },
      { title: "Resumes", text: "A dropped transfer picks up where it stopped." },
      { title: "From the console too", text: "Browse, download or remove Inbox files from the browser." },
    ],
    steps: [
      { title: "Send", text: "Run pb send ./report.pdf --to homelab." },
      { title: "Receive", text: "It lands in ~/Paperboat/Inbox on the other machine." },
      { title: "Manage", text: "Run pb inbox, or open Files in the console." },
    ],
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
    headline: "Secrets your servers can read, and we can’t.",
    summary: "Environment variables, encrypted end to end and delivered to the machines that need them.",
    benefits: [
      { title: "End to end", text: "Values are encrypted on your device. Our servers only see ciphertext." },
      { title: "Delivered automatically", text: "Commands on the machine get the right variables when they start." },
      { title: "Easy to rotate", text: "Change a value once; every machine gets the new one." },
    ],
    steps: [
      { title: "Set a value", text: "Run pb env set, or add it in the console." },
      { title: "Pick machines", text: "Choose which machines get it." },
      { title: "Run your app", text: "The variables are there when it starts." },
    ],
    example: {
      title: "Store a secret, encrypted before it leaves your machine",
      text: ["$ pb env set API_KEY", "$ pb env list"].join("\n"),
    },
    compare: [],
  },
  {
    slug: "config-sync",
    name: "Config sync",
    headline: "The same setup on every machine.",
    summary: "Keep dotfiles and settings in sync from a Git repository.",
    benefits: [
      { title: "From Git", text: "Use any HTTPS, SSH or local repository." },
      { title: "New machines ready", text: "A machine that joins gets your setup right away." },
      { title: "Conflicts shown", text: "If a file changed in two places, you see both and pick." },
    ],
    steps: [
      { title: "Connect a repo", text: "Point Paperboat at your dotfiles repository." },
      { title: "Assign machines", text: "Choose which machines follow it." },
      { title: "Keep working", text: "Changes sync both ways." },
    ],
    example: {
      title: "Keep dotfiles the same on every machine",
      text: ["$ pb config status", "$ pb config sync"].join("\n"),
    },
    compare: [],
  },
  {
    slug: "teams",
    name: "Teams",
    headline: "Share with your team, not everything.",
    summary: "Your machines stay yours. Share exactly what a team needs.",
    benefits: [
      { title: "Share one thing", text: "A machine, a terminal, a preview or some secrets." },
      { title: "Roles", text: "Owners, admins and members, with approval for new machines." },
      { title: "See what happened", text: "An activity log shows who did what." },
    ],
    steps: [
      { title: "Create a team", text: "Run pb team create or use the console." },
      { title: "Invite people", text: "Send invites by email." },
      { title: "Share", text: "Share machines, terminals, previews and secrets into the team." },
    ],
    example: {
      title: "Invite someone and share a machine",
      text: ["$ pb team invite my-team ana@example.com", "$ pb team machine grant my-team machine_8a1d5c2e-3b47-4f0e-9d62-1c7e5b0a4f93"].join("\n"),
    },
    compare: ["tailscale", "teleport", "netbird"],
  },
  {
    slug: "self-hosting",
    name: "Self-hosting",
    headline: "Run the network on your own servers.",
    summary: "Host your own relay and tunnel server, on your own domain.",
    benefits: [
      { title: "Your servers", text: "Traffic goes through boxes you run." },
      { title: "One command", text: "The installer sets everything up on Linux." },
      { title: "Not counted", text: "Traffic through your own servers doesn’t use your plan’s bandwidth." },
    ],
    steps: [
      { title: "Install", text: "Run the self-host installer on a Linux server." },
      { title: "Add it", text: "Add it to your workspace from the console." },
      { title: "Done", text: "Your machines start using it." },
    ],
    example: {
      title: "Run your own relay or tunnel server",
      text: "$ curl -fsSL {selfhostInstallUrl} | sh",
    },
    compare: ["netbird", "cloudflare-tunnel"],
  },
]
