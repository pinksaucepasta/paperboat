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
  /** Three short blocks. */
  benefits: { title: string; text: string }[]
  example: { title: string; text: string }
  /** Exactly three steps. */
  steps: { title: string; text: string }[]
  /** How it connects to other products: a product slug and one sentence. */
  related: { slug: string; text: string }[]
  /** Compare pages that apply, by slug (see compare-data.ts). */
  compare: string[]
}

export const products: Product[] = [
  {
    slug: "terminals",
    name: "Terminals",
    headline: "Terminals that keep running",
    summary: "Your shell runs on the machine, not in your laptop’s connection.",
    benefits: [
      { title: "Survives drops", text: "Close the lid, lose Wi-Fi or switch networks. The shell keeps running." },
      { title: "See what you missed", text: "Output from while you were away is replayed." },
      { title: "Anywhere", text: "Open the same terminal from a terminal app, a browser or a phone." },
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
    related: [
      { slug: "ssh", text: "Use VS Code, scp or rsync over plain SSH." },
      { slug: "teams", text: "Share a live session, view-only or interactive." },
      { slug: "previews", text: "Run a dev server, then share its port with a link." },
    ],
    compare: ["tailscale", "teleport"],
  },
  {
    slug: "ssh",
    name: "SSH",
    headline: "Plain SSH, no key chores",
    summary: "Use pb ssh, or your usual ssh, scp and rsync.",
    benefits: [
      { title: "Works with your tools", text: "VS Code, Cursor, Zed, scp, rsync and anything else that speaks SSH." },
      { title: "Keys handled", text: "Paperboat sets up and rotates keys. No authorized_keys editing." },
      { title: "No open ports", text: "No public IP or open SSH port." },
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
    related: [
      { slug: "terminals", text: "Want a shell that survives drops? Open a terminal." },
      { slug: "private-access", text: "Reach databases and internal apps with no open ports." },
      { slug: "files", text: "Send one file with pb send. No scp setup." },
    ],
    compare: ["tailscale", "teleport"],
  },
  {
    slug: "previews",
    name: "Previews",
    headline: "Share a port in one command",
    summary: "pb preview gives an app on your machine a public link.",
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
    related: [
      { slug: "tunnels", text: "Need a lasting address? Use a tunnel." },
      { slug: "terminals", text: "Start your app in a terminal that keeps running." },
      { slug: "teams", text: "Limit a link to your team." },
    ],
    compare: ["ngrok", "cloudflare-tunnel"],
  },
  {
    slug: "tunnels",
    name: "Tunnels",
    headline: "A permanent address for a service",
    summary: "A tunnel keeps its address through restarts and can use your own domain.",
    benefits: [
      { title: "Stays put", text: "The address survives machine and app restarts." },
      { title: "Your domain", text: "Add your domain. We handle certificates." },
      { title: "Many routes", text: "Route paths or ports on one tunnel to different apps." },
    ],
    steps: [
      { title: "Create", text: "Run pb tunnel create api --port 8080." },
      { title: "Point a domain", text: "Add your domain and follow the DNS steps." },
      { title: "Leave it running", text: "It keeps serving through restarts and updates." },
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
    related: [
      { slug: "previews", text: "For a temporary link, use a preview." },
      { slug: "self-hosting", text: "Serve tunnels from your own server and domain." },
      { slug: "teams", text: "Share a tunnel with your team." },
    ],
    compare: ["ngrok", "cloudflare-tunnel"],
  },
  {
    slug: "private-access",
    name: "Private access",
    headline: "Reach any port on your machines",
    summary: "Each machine gets a local address only your other machines can reach.",
    benefits: [
      { title: "Any protocol", text: "Databases, Redis, internal web apps. Anything on TCP." },
      { title: "Encrypted end to end", text: "Only your machines can read the traffic." },
      { title: "Nothing exposed", text: "No public ports, no firewall changes." },
    ],
    steps: [
      { title: "Install on both machines", text: "Run the installer on each one." },
      { title: "Find the address", text: "Run pb machine list for each machine’s 127.100.x.x address." },
      { title: "Connect", text: "Point your app at that address and port." },
    ],
    example: {
      title: "Reach a port on a machine without publishing it",
      text: "$ pb access machine studio --port 5432",
    },
    related: [
      { slug: "ssh", text: "SSH is the most common use." },
      { slug: "self-hosting", text: "Run your own relay for traffic that can’t go direct." },
      { slug: "teams", text: "Give teammates access without exposing the machine." },
    ],
    compare: ["tailscale", "netbird"],
  },
  {
    slug: "files",
    name: "Files",
    headline: "Send files between your machines",
    summary: "pb send puts a file in another machine’s Inbox.",
    benefits: [
      { title: "Straight to the machine", text: "Files go machine to machine, encrypted. We don’t store them." },
      { title: "Resumes", text: "A dropped transfer resumes." },
      { title: "From the console too", text: "Browse, download or remove Inbox files in the browser." },
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
    related: [
      { slug: "private-access", text: "Need a service, not a file? Reach its port." },
      { slug: "teams", text: "Send to a teammate’s machine once it’s shared with you." },
      { slug: "ssh", text: "scp and rsync work with managed keys." },
    ],
    compare: ["tailscale"],
  },
  {
    slug: "env",
    name: "Environment variables",
    headline: "Secrets your machines read and we can’t",
    summary: "Environment variables, encrypted end to end and delivered to your machines.",
    benefits: [
      { title: "End to end", text: "Values are encrypted on your device. We only see ciphertext." },
      { title: "Delivered automatically", text: "Commands get the right variables when they start." },
      { title: "Easy to rotate", text: "Change a value once. Every machine gets it." },
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
    related: [
      { slug: "config-sync", text: "Sync dotfiles. Keep secrets in ENV, not the repo." },
      { slug: "teams", text: "Share secrets with a team’s machines." },
      { slug: "terminals", text: "Variables are set when a terminal or pb exec starts." },
    ],
    compare: [],
  },
  {
    slug: "config-sync",
    name: "Config sync",
    headline: "The same setup on every machine",
    summary: "Sync dotfiles and settings from a Git repository.",
    benefits: [
      { title: "From Git", text: "Use any HTTPS, SSH or local repository." },
      { title: "New machines ready", text: "A new machine gets your setup right away." },
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
    related: [
      { slug: "env", text: "Keep secrets in encrypted variables, not the repo." },
      { slug: "teams", text: "Give a teammate’s machine the same setup." },
      { slug: "terminals", text: "Your shell settings are in every terminal." },
    ],
    compare: [],
  },
  {
    slug: "teams",
    name: "Teams",
    headline: "Share with your team, not everything",
    summary: "Your machines stay yours. Share only what a team needs.",
    benefits: [
      { title: "Share one thing", text: "A machine, a terminal, a preview or some secrets." },
      { title: "Roles", text: "Owners, admins and members. New machines need approval." },
      { title: "See what happened", text: "An activity log shows who did what." },
    ],
    steps: [
      { title: "Create a team", text: "Run pb team create or use the console." },
      { title: "Invite people", text: "Send invites by email." },
      { title: "Share", text: "Share machines, terminals, previews and secrets." },
    ],
    example: {
      title: "Invite someone and share a machine",
      text: ["$ pb team invite my-team ana@example.com", "$ pb team machine grant my-team machine_8a1d5c2e-3b47-4f0e-9d62-1c7e5b0a4f93"].join("\n"),
    },
    related: [
      { slug: "terminals", text: "Share a terminal session." },
      { slug: "previews", text: "Limit a preview link to your team." },
      { slug: "env", text: "Share secrets with team machines, encrypted end to end." },
    ],
    compare: ["tailscale", "teleport", "netbird"],
  },
  {
    slug: "self-hosting",
    name: "Self-hosting",
    headline: "Run it on your own servers",
    summary: "Host your own relay and tunnel server on your domain.",
    benefits: [
      { title: "Your servers", text: "Traffic goes through servers you run." },
      { title: "One command", text: "The installer sets up Linux." },
      { title: "Not counted", text: "Traffic through your servers doesn’t use plan bandwidth." },
    ],
    steps: [
      { title: "Install", text: "Run the self-host installer on a Linux server." },
      { title: "Add it", text: "Add it in the console." },
      { title: "Done", text: "Your machines start using it." },
    ],
    example: {
      title: "Run your own relay or tunnel server",
      text: "$ curl -fsSL {selfhostInstallUrl} | sh",
    },
    related: [
      { slug: "tunnels", text: "Your tunnel server serves your domains." },
      { slug: "private-access", text: "Your relay carries traffic that can’t go direct." },
      { slug: "previews", text: "Previews can come from your edge too." },
    ],
    compare: ["netbird", "cloudflare-tunnel"],
  },
]
