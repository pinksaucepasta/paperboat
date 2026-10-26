/**
 * The compare pages: /compare (pick up to two products and see them beside Paperboat) and one
 * long page per product (pages/compare/[slug].astro). Every claim about another product links to
 * the page of its own documentation, pricing page or repository that says it; claims were
 * checked on 2026-10-23 and must be rechecked before each release of the site.
 * "No" means the product's documentation describes no such feature.
 */

export type RowId =
  | "terminal_persistent"
  | "terminal_replay"
  | "terminal_browser"
  | "terminal_share"
  | "exec"
  | "ssh_keys"
  | "ssh_tools"
  | "ssh_vscode"
  | "files_send"
  | "files_inbox"
  | "env_secrets"
  | "env_where"
  | "config_sync"
  | "preview_url"
  | "preview_inspect"
  | "tunnel_permanent"
  | "tunnel_domains"
  | "tunnel_protocols"
  | "private_network"
  | "encryption_native"
  | "encryption_public"
  | "teams"
  | "access_rules"
  | "api_tokens"
  | "selfhost"
  | "platforms"
  | "pricing_model"
  | "free_tier"

export type Section = { id: string; title: string; sentence: string; rows: { id: RowId; label: string }[] }

/** What people decide on: ten rows in four groups. */
export const sections: Section[] = [
  {
    id: "network",
    title: "Network and encryption",
    sentence: "How machines connect and who can read the traffic.",
    rows: [
      { id: "encryption_native", label: "End-to-end encryption, direct when possible" },
      { id: "private_network", label: "Machines reach each other privately" },
      { id: "selfhost", label: "Self-hosted relay" },
    ],
  },
  {
    id: "terminals",
    title: "Terminals and SSH",
    sentence: "Work on a machine from anywhere.",
    rows: [
      { id: "terminal_persistent", label: "Terminal survives disconnects" },
      { id: "terminal_browser", label: "Terminal in the browser" },
      { id: "terminal_share", label: "Shareable terminals" },
      { id: "ssh_keys", label: "Managed SSH" },
    ],
  },
  {
    id: "publish",
    title: "Previews and secrets",
    sentence: "Share work and keep secrets.",
    rows: [
      { id: "preview_url", label: "Previews and tunnels" },
      { id: "env_secrets", label: "Encrypted environment variables" },
    ],
  },
  {
    id: "pricing",
    title: "Pricing",
    sentence: "How you pay.",
    rows: [{ id: "pricing_model", label: "How you pay" }],
  },
]

/** A cell: a short, specific answer and, for other products, the page that says it. */
export type Cell = { text: string; source?: string }
type Cells = Record<RowId, Cell>

/** Builds cells from `[text, source key]` pairs and the product's source pages. */
function cells(sources: Record<string, string>, values: Record<RowId, [string, string?]>): Cells {
  return Object.fromEntries(
    Object.entries(values).map(([row, [text, key]]) => {
      if (key && !sources[key]) throw new Error(`compare-data: unknown source ${key} for ${row}`)
      return [row, { text, source: key ? sources[key] : undefined }]
    }),
  ) as Cells
}

/** Paperboat's own answers (PLAN.md; "coming" where the feature is built but not released). */
export const paperboat = {
  name: "Paperboat",
  cells: cells({}, {
    terminal_persistent: ["Yes, runs on the machine"],
    terminal_replay: ["Yes, replays what you missed"],
    terminal_browser: ["Yes, in the console, on phones too"],
    terminal_share: ["Yes, view-only or interactive"],
    exec: ["Yes, pb exec, with exit codes"],
    ssh_keys: ["Yes, keys and host trust handled"],
    ssh_tools: ["Yes, pb ssh, scp, sftp, rsync and plain ssh"],
    ssh_vscode: ["Yes, through generated SSH host names"],
    files_send: ["Yes, pb send, resumable"],
    files_inbox: ["Yes, an Inbox on each machine"],
    env_secrets: ["Yes, end-to-end encrypted"],
    env_where: ["Terminals and pb exec; SSH coming"],
    config_sync: ["Yes, from your Git repository"],
    preview_url: ["Yes, pb preview 3000"],
    preview_inspect: ["Coming: inspector in the console"],
    tunnel_permanent: ["Yes"],
    tunnel_domains: ["Yes, on every plan"],
    tunnel_protocols: ["HTTP/1.1, HTTP/2, HTTP/3, WebSocket"],
    private_network: ["Yes, a private name per machine"],
    encryption_native: ["Yes, direct when possible"],
    encryption_public: ["TLS ends at Paperboat's edge"],
    teams: ["Yes, owners, admins, members"],
    access_rules: ["Per machine; tags and rules coming"],
    api_tokens: ["Coming"],
    selfhost: ["Yes, relays and tunnel servers"],
    platforms: ["Linux, macOS, any browser; Windows coming"],
    pricing_model: ["Bandwidth; Team per seat"],
    free_tier: ["Everything for one person, 5 GB"],
  }),
  pricing: [
    "Free: $0, everything for one person, 5 GB of relay and edge bandwidth a month",
    "Pro: $8 a month or $80 a year, 100 GB",
    "Team: $15 per seat a month or $150 a year, 100 GB per seat; 14-day trial",
    "Direct traffic between your machines is never counted",
  ],
}

/** One point where the two products differ: a heading, a paragraph or two, and the commands. */
export type Difference = { title: string; body: string[]; them?: string; us?: string }

export type Product = {
  slug: string
  /** As in a sentence: "Paperboat vs Tailscale", "Switching from an enterprise VPN". */
  name: string
  /** Column heading on /compare. */
  label: string
  summary: string
  cells: Cells
  differences: Difference[]
  chooseThem: string[]
  chooseUs: string[]
  pricing: { lines: string[]; source: string }
  switching: string[]
  faq: { q: string; a: string }[]
}

const install = "Install Paperboat on each machine with the one-line installer, then sign in."

// Tailscale — sources opened 2026-10-23.
const tailscaleSources = {
  ssh: "https://tailscale.com/kb/1193/tailscale-ssh",
  console: "https://tailscale.com/kb/1216/tailscale-ssh-console",
  funnel: "https://tailscale.com/kb/1223/funnel",
  taildrop: "https://tailscale.com/kb/1106/taildrop",
  policy: "https://tailscale.com/kb/1337/policy-syntax",
  keys: "https://tailscale.com/kb/1085/auth-keys",
  derp: "https://tailscale.com/kb/1118/custom-derp-servers",
  download: "https://tailscale.com/download",
  pricing: "https://tailscale.com/pricing",
}

// ngrok
const ngrokSources = {
  agent: "https://ngrok.com/docs/agent/",
  start: "https://ngrok.com/docs/getting-started/",
  domains: "https://ngrok.com/docs/universal-gateway/domains/",
  tls: "https://ngrok.com/docs/universal-gateway/tls-termination/",
  download: "https://ngrok.com/download",
  pricing: "https://ngrok.com/pricing",
}

// Cloudflare Tunnel (cloudflared on its own)
const cfTunnelSources = {
  tunnel: "https://developers.cloudflare.com/tunnel/",
  quick: "https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/",
  setup: "https://developers.cloudflare.com/tunnel/setup/",
  routing: "https://developers.cloudflare.com/tunnel/routing/",
  private: "https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/",
  api: "https://developers.cloudflare.com/tunnel/setup/",
  limits: "https://developers.cloudflare.com/cloudflare-one/account-limits/",
}

// Teleport
const teleportSources = {
  matrix: "https://goteleport.com/docs/feature-matrix/",
  tsh: "https://goteleport.com/docs/reference/cli/tsh/",
  pricing: "https://goteleport.com/pricing/",
}

// NetBird
const netbirdSources = {
  docs: "https://docs.netbird.io/",
  ssh: "https://docs.netbird.io/how-to/ssh",
  pricing: "https://netbird.io/pricing",
  plans: "https://docs.netbird.io/manage/settings/plans-and-billing",
  repo: "https://github.com/netbirdio/netbird",
}

// ZeroTier
const zerotierSources = {
  protocol: "https://docs.zerotier.com/protocol",
  controller: "https://docs.zerotier.com/controller",
  orgs: "https://docs.zerotier.com/organizations/",
  download: "https://www.zerotier.com/download/",
  pricing: "https://www.zerotier.com/pricing/",
}

// Twingate
const twingateSources = {
  ssh: "https://www.twingate.com/docs/ssh-privileged-access-overview",
  resources: "https://www.twingate.com/docs/resources",
  relays: "https://www.twingate.com/docs/understanding-relays",
  users: "https://www.twingate.com/docs/users",
  api: "https://www.twingate.com/docs/api-overview",
  connectors: "https://www.twingate.com/docs/connector-deployment",
  clients: "https://www.twingate.com/docs/clients",
  faq: "https://www.twingate.com/docs/faq",
  pricing: "https://www.twingate.com/pricing",
}

// Cloudflare Zero Trust (Access + WARP + Tunnel)
const cfZeroTrustSources = {
  setup: "https://developers.cloudflare.com/cloudflare-one/setup/",
  ssh: "https://developers.cloudflare.com/cloudflare-one/tutorials/ssh/",
  tunnel: "https://developers.cloudflare.com/tunnel/",
  routing: "https://developers.cloudflare.com/tunnel/routing/",
  private: "https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/private-net/",
  policies: "https://developers.cloudflare.com/cloudflare-one/policies/access/",
  users: "https://developers.cloudflare.com/cloudflare-one/team-and-resources/users/seat-management/",
  api: "https://developers.cloudflare.com/api/",
  warp: "https://developers.cloudflare.com/cloudflare-one/connections/connect-devices/warp/",
  pricing: "https://www.cloudflare.com/plans/zero-trust-services/",
  limits: "https://developers.cloudflare.com/cloudflare-one/account-limits/",
}

// Enterprise VPN / ZTNA
const vpnSources = {
  zpa: "https://www.zscaler.com/products-and-solutions/zscaler-private-access",
  prisma: "https://www.paloaltonetworks.com/sase/access",
  cisco: "https://www.cisco.com/site/us/en/products/security/secure-client/index.html",
  openvpn: "https://openvpn.net/access-server/",
  openvpnPricing: "https://openvpn.net/access-server/pricing/",
}

// SSH and a bastion
const sshSources = {
  ssh: "https://man.openbsd.org/ssh",
  config: "https://man.openbsd.org/ssh_config",
  sshd: "https://man.openbsd.org/sshd_config",
  scp: "https://man.openbsd.org/scp",
  tmux: "https://github.com/tmux/tmux/wiki",
  openssh: "https://www.openssh.com/",
}

// VS Code Remote Tunnels and dev tunnels
const vscodeSources = {
  tunnels: "https://code.visualstudio.com/docs/remote/tunnels",
  overview: "https://code.visualstudio.com/docs/remote/remote-overview",
  ports: "https://code.visualstudio.com/docs/debugtest/port-forwarding",
  cli: "https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/cli-commands",
  devtunnels: "https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/overview",
  security: "https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/security",
  limits: "https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/azure-subscription-service-limits",
  repo: "https://github.com/microsoft/dev-tunnels",
}

/** Paperboat's side of the differences that recur, written once. */
const ours = {
  terminals:
    "Paperboat terminals run on the machine and keep running when your laptop sleeps or the network drops. Reconnect from any computer or a phone browser and you see what you missed. Others can watch or type along.",
  previews:
    "pb preview gives a port a public HTTPS link. pb tunnel gives it a permanent address on your own domain. Both are on every plan.",
  files:
    "pb send puts files in the other machine's Inbox and resumes after a drop. ENV delivers encrypted variables to your commands and terminals. Config sync keeps settings the same everywhere from a Git repository.",
  install:
    "Paperboat is one install per machine and a sign-in. No network, gateway or proxy to plan.",
}

export const products: Product[] = [
  {
    slug: "tailscale",
    name: "Tailscale",
    label: "Tailscale",
    summary:
      "Tailscale puts your devices on one private WireGuard network, with detailed access rules and clients for every platform. Paperboat also connects your machines privately and adds terminals that survive disconnects, SSH, files, encrypted variables, previews and tunnels.",
    cells: cells(tailscaleSources, {
      terminal_persistent: ["No, a standard SSH session", "ssh"],
      terminal_replay: ["No", "ssh"],
      terminal_browser: ["Yes, SSH Console in the admin console (beta)", "console"],
      terminal_share: ["No", "ssh"],
      exec: ["Through ssh", "ssh"],
      ssh_keys: ["Yes, with Tailscale SSH", "ssh"],
      ssh_tools: ["Yes, over the private network", "ssh"],
      ssh_vscode: ["Yes, as plain SSH over the network", "ssh"],
      files_send: ["Yes, Taildrop between your own devices", "taildrop"],
      files_inbox: ["No; files land in Downloads", "taildrop"],
      env_secrets: ["No", "pricing"],
      env_where: ["—"],
      config_sync: ["No", "pricing"],
      preview_url: ["Yes, tailscale funnel 3000", "funnel"],
      preview_inspect: ["No", "funnel"],
      tunnel_permanent: ["Yes, on your tailnet's ts.net name", "funnel"],
      tunnel_domains: ["No, ts.net names only", "funnel"],
      tunnel_protocols: ["HTTPS on ports 443, 8443, 10000", "funnel"],
      private_network: ["Yes, a WireGuard mesh", "pricing"],
      encryption_native: ["End to end (WireGuard)", "funnel"],
      encryption_public: ["TLS ends on your device", "funnel"],
      teams: ["Yes, owner, admin and other roles", "policy"],
      access_rules: ["Yes, grants, ACLs, groups and tags", "policy"],
      api_tokens: ["Yes, API keys and OAuth clients", "keys"],
      selfhost: ["Your own DERP relays", "derp"],
      platforms: ["Linux, macOS, Windows, iOS, Android", "download"],
      pricing_model: ["Per user: $8 or $18 a month", "pricing"],
      free_tier: ["Personal: 6 users, unlimited devices", "pricing"],
    }),
    differences: [
      {
        title: "A network, or a place to work",
        body: [
          "Tailscale gives each device an address on a private network. You bring the tools: ssh, tmux, scp.",
          ours.install,
        ],
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "A Tailscale SSH session is a normal SSH session. When the connection drops, the shell ends unless you started tmux. SSH Console opens a shell in the admin console for owners and admins, and ends when the tab closes.",
          ours.terminals,
        ],
        them: "$ ssh devbox\n$ tmux new -A -s work",
        us: "$ pb devbox\n# later, from any device\n$ pb connect devbox",
      },
      {
        title: "Public links",
        body: [
          "Funnel shares a local HTTPS service on your tailnet's ts.net name, on ports 443, 8443 or 10000. It doesn't support your own domains.",
          ours.previews,
        ],
        them: "$ tailscale funnel 3000",
        us: "$ pb preview 3000\n$ pb tunnel create api --port 3000",
      },
      {
        title: "Files, secrets and settings",
        body: [
          "Taildrop sends files between your devices into Downloads. Tailscale has no environment variables or config sync.",
          ours.files,
        ],
        them: "$ tailscale file cp report.pdf laptop:",
        us: "$ pb send report.pdf --to laptop",
      },
      {
        title: "Access rules for a whole company",
        body: [
          "Tailscale's policy file (grants, ACLs, groups, tags) and phone clients are mature and cover whole companies.",
          "Paperboat gives team members access per machine. Tags and access rules are being built. For company-wide network policy today, choose Tailscale.",
        ],
      },
    ],
    chooseThem: [
      "You want every device, including phones and whole subnets, on one private network.",
      "You need detailed network access rules for a company today.",
    ],
    chooseUs: [
      "You mostly want to work on your machines: terminals, SSH, files and secrets.",
      "You want public previews and permanent tunnels with your own domains in the same tool.",
    ],
    pricing: {
      lines: [
        "Personal: free, up to 6 users, unlimited devices",
        "Standard: $8 per user a month",
        "Premium: $18 per user a month",
        "Enterprise: by quote",
      ],
      source: tailscaleSources.pricing,
    },
    switching: [
      install,
      "Use pb ssh or the generated SSH host names where you used Tailscale addresses, and start your long-running work in a Paperboat terminal.",
      "Move public links from Funnel to pb preview or a tunnel.",
      "Keep Tailscale running until nothing depends on it; the two work side by side.",
    ],
    faq: [
      {
        q: "Can I use Paperboat and Tailscale together?",
        a: "Yes. They don't conflict: Paperboat uses its own private names, and you can keep Tailscale for the rest of your network.",
      },
      {
        q: "Is Paperboat a VPN?",
        a: "Not a general one. Your machines reach each other privately, but Paperboat doesn't route whole subnets or carry phones' traffic; it connects the machines you install it on.",
      },
      {
        q: "Does Paperboat encrypt end to end like WireGuard?",
        a: "Yes, between your own devices, and directly when the network allows; relays only forward encrypted traffic. Public links end TLS at Paperboat's edge.",
      },
    ],
  },
  {
    slug: "ngrok",
    name: "ngrok",
    label: "ngrok",
    summary:
      "ngrok puts a local service on a public URL, with a request inspector and traffic policies. Paperboat also does previews and permanent tunnels, plus terminals, SSH, files and secrets.",
    cells: cells(ngrokSources, {
      terminal_persistent: ["No", "agent"],
      terminal_replay: ["No", "agent"],
      terminal_browser: ["No", "agent"],
      terminal_share: ["No", "agent"],
      exec: ["No", "agent"],
      ssh_keys: ["No", "agent"],
      ssh_tools: ["Only as a TCP endpoint", "agent"],
      ssh_vscode: ["No", "agent"],
      files_send: ["No", "agent"],
      files_inbox: ["No", "agent"],
      env_secrets: ["No", "agent"],
      env_where: ["—"],
      config_sync: ["No", "agent"],
      preview_url: ["Yes, ngrok http 3000", "start"],
      preview_inspect: ["Yes, traffic inspector with replay", "start"],
      tunnel_permanent: ["Yes, a fixed dev domain; more on paid plans", "domains"],
      tunnel_domains: ["Yes, from Pay-as-you-go", "pricing"],
      tunnel_protocols: ["HTTP, HTTPS, TCP, TLS", "pricing"],
      private_network: ["No", "agent"],
      encryption_native: ["—"],
      encryption_public: ["TLS ends at ngrok; end-to-end TLS optional", "tls"],
      teams: ["Yes, users from Pay-as-you-go", "pricing"],
      access_rules: ["IP policies; roles as a paid add-on", "pricing"],
      api_tokens: ["Yes, API keys", "pricing"],
      selfhost: ["No", "agent"],
      platforms: ["Linux, macOS, Windows, FreeBSD, Docker", "download"],
      pricing_model: ["Plan plus usage: $10 or $20 a month", "pricing"],
      free_tier: ["3 endpoints, 1 GB out, 20k requests", "pricing"],
    }),
    differences: [
      {
        title: "A port, or the whole machine",
        body: [
          "ngrok gets traffic to a service. It gives you no shell, files or secrets on the machine behind it.",
          "Paperboat gives you the link and the machine: open a terminal, copy files, run commands, from your laptop or a browser.",
        ],
        them: "$ ngrok http 3000",
        us: "$ pb preview 3000",
      },
      {
        title: "Your own domains",
        body: [
          "On ngrok, your own domain starts at Pay-as-you-go. Free has one development domain and Hobbyist has ngrok-branded ones.",
          "Paperboat tunnels take your own domains on every plan, Free included. We show the DNS records and issue the certificate.",
        ],
        them: "$ ngrok http 3000 --url api.example.com",
        us: "$ pb tunnel domain add api api.example.com",
      },
      {
        title: "Request inspection",
        body: [
          "ngrok's traffic inspector shows each request and lets you replay it. It is one of its strongest features.",
          "Paperboat's inspector, with replay, is being built into the console. Until it ships, ngrok is ahead.",
        ],
      },
      {
        title: "How you pay",
        body: [
          "ngrok plans charge per user, per active domain hour, and for data and requests beyond the plan.",
          "Paperboat plans differ only in relay and edge bandwidth. Previews, tunnels and domains cost nothing extra, and direct traffic is free.",
        ],
      },
    ],
    chooseThem: [
      "You only need public URLs, with traffic features such as policies, load balancing and an API gateway.",
      "You want request inspection and replay today.",
    ],
    chooseUs: [
      "You also want to reach and work on the machine itself.",
      "You want your own domains and permanent tunnels on a free plan.",
    ],
    pricing: {
      lines: [
        "Free: $0, 3 endpoints, 1 GB out, 20,000 requests a month",
        "Hobbyist: $10 a month, 5 GB, ngrok-branded domains",
        "Pay-as-you-go: $20 a month plus usage; your own domains",
        "Enterprise: by quote",
      ],
      source: ngrokSources.pricing,
    },
    switching: [
      install,
      "Replace ngrok http <port> with pb preview <port> for short-lived links.",
      "For lasting addresses, create a tunnel and move your domain's CNAME record to it.",
      "Stop the ngrok agent once nothing points at it.",
    ],
    faq: [
      {
        q: "Does Paperboat support WebSockets and HTTP/2?",
        a: "Yes: HTTP/1.1, HTTP/2, HTTP/3 and WebSocket go through previews and tunnels.",
      },
      {
        q: "Can I keep a preview private?",
        a: "Yes. A preview can be public, only for you, or for your team.",
      },
      {
        q: "Does Paperboat have raw TCP endpoints?",
        a: "Public links are for web traffic (HTTP and WebSocket). For other protocols, your own machines reach each other's ports privately.",
      },
    ],
  },
  {
    slug: "cloudflare-tunnel",
    name: "Cloudflare Tunnel",
    label: "Cloudflare Tunnel",
    summary:
      "Cloudflare Tunnel connects a service to Cloudflare's network, behind Cloudflare's protection, on your Cloudflare domain. Paperboat gives you tunnels and previews without moving your DNS, plus terminals, SSH, files and secrets.",
    cells: cells(cfTunnelSources, {
      terminal_persistent: ["No", "tunnel"],
      terminal_replay: ["No", "tunnel"],
      terminal_browser: ["With Cloudflare Access", "private"],
      terminal_share: ["No", "tunnel"],
      exec: ["No", "tunnel"],
      ssh_keys: ["With Cloudflare Access", "private"],
      ssh_tools: ["Yes, with cloudflared on the client", "setup"],
      ssh_vscode: ["With cloudflared as a proxy", "setup"],
      files_send: ["No", "tunnel"],
      files_inbox: ["No", "tunnel"],
      env_secrets: ["No", "tunnel"],
      env_where: ["—"],
      config_sync: ["No", "tunnel"],
      preview_url: ["Yes, quick tunnels on trycloudflare.com", "quick"],
      preview_inspect: ["No", "tunnel"],
      tunnel_permanent: ["Yes, named tunnels", "private"],
      tunnel_domains: ["Yes, on a domain using Cloudflare DNS", "routing"],
      tunnel_protocols: ["HTTP, HTTPS, TCP, SSH, RDP and more", "routing"],
      private_network: ["With the WARP client", "private"],
      encryption_native: ["Through Cloudflare's network", "tunnel"],
      encryption_public: ["Traffic passes through Cloudflare's proxy", "tunnel"],
      teams: ["Through Cloudflare Zero Trust", "private"],
      access_rules: ["Through Access and Gateway policies", "private"],
      api_tokens: ["Yes, Cloudflare API tokens", "api"],
      selfhost: ["The connector only", "private"],
      platforms: ["Linux, macOS, Windows, Docker", "setup"],
      pricing_model: ["Available on all plans", "tunnel"],
      free_tier: ["Quick tunnels need no account", "quick"],
    }),
    differences: [
      {
        title: "Your DNS stays where it is",
        body: [
          "A named Cloudflare Tunnel serves a hostname on a domain whose DNS is on Cloudflare. Using your domain means moving its DNS there.",
          "Paperboat asks for two CNAME records at your current DNS provider and issues the certificate.",
        ],
        them: "$ cloudflared tunnel route dns api api.example.com",
        us: "$ pb tunnel domain add api api.example.com",
      },
      {
        title: "Quick links",
        body: [
          "Quick tunnels give a random trycloudflare.com address with no account, for testing.",
          "pb preview gives a named link on Paperboat's preview domain. Make it public, private to you or open to your team, with a time limit you choose.",
        ],
        them: "$ cloudflared tunnel --url http://localhost:3000",
        us: "$ pb preview 3000 --ttl 2h",
      },
      {
        title: "Working on the machine",
        body: [
          "Cloudflare Tunnel connects services. Reaching a shell takes Cloudflare Access and cloudflared on the client.",
          ours.terminals,
        ],
      },
      {
        title: "Cloudflare in front",
        body: [
          "Tunnel traffic flows through Cloudflare's network, with its CDN, firewall and DDoS protection. For a public site that needs them, that is a real advantage.",
          "Paperboat's edge ends TLS for public links and forwards to your machine. It doesn't cache or filter traffic.",
        ],
      },
    ],
    chooseThem: [
      "Your domain is already on Cloudflare and you want its caching and protection in front.",
      "You are serving a public production site at scale.",
    ],
    chooseUs: [
      "You don't want to move your DNS.",
      "You want terminals, SSH, files and previews on the same machines, not only tunnels.",
    ],
    pricing: {
      lines: ["Cloudflare Tunnel: available on all plans", "Quick tunnels: no account needed"],
      source: cfTunnelSources.tunnel,
    },
    switching: [
      install,
      "Create a Paperboat tunnel for each named tunnel and add its routes.",
      "Add your domain to the tunnel and update the CNAME records where your DNS lives.",
      "Remove the cloudflared service once traffic has moved.",
    ],
    faq: [
      {
        q: "Do I need to move my domain to Paperboat?",
        a: "No. You add two CNAME records wherever your DNS is hosted; Paperboat checks them and issues the certificate.",
      },
      {
        q: "Does Paperboat protect against DDoS like Cloudflare?",
        a: "Paperboat's edge isn't a CDN or a web firewall. For a public site that needs that, put a CDN in front of the tunnel's domain or keep Cloudflare.",
      },
    ],
  },
  {
    slug: "teleport",
    name: "Teleport",
    label: "Teleport",
    summary:
      "Teleport is a company access platform for SSH, Kubernetes, databases and internal apps, with short-lived certificates, session recording and audit. Paperboat is lighter: install it and work in a minute, with terminals that survive disconnects, previews and tunnels.",
    cells: cells(teleportSources, {
      terminal_persistent: ["No", "matrix"],
      terminal_replay: ["Recorded sessions play back afterwards", "tsh"],
      terminal_browser: ["Yes, in the web UI", "matrix"],
      terminal_share: ["Yes, join sessions; moderation in Enterprise", "tsh"],
      exec: ["Yes, tsh ssh host command", "tsh"],
      ssh_keys: ["Yes, short-lived certificates", "matrix"],
      ssh_tools: ["Yes, tsh and OpenSSH with tsh config", "tsh"],
      ssh_vscode: ["Yes, with tsh config", "tsh"],
      files_send: ["Through tsh scp", "tsh"],
      files_inbox: ["No", "matrix"],
      env_secrets: ["No", "matrix"],
      env_where: ["—"],
      config_sync: ["No", "matrix"],
      preview_url: ["No, internal apps only", "matrix"],
      preview_inspect: ["No", "matrix"],
      tunnel_permanent: ["No public tunnels", "matrix"],
      tunnel_domains: ["No public tunnels", "matrix"],
      tunnel_protocols: ["SSH, Kubernetes, databases, apps, desktops", "matrix"],
      private_network: ["No; access goes through its proxy", "matrix"],
      encryption_native: ["TLS to Teleport's proxy", "matrix"],
      encryption_public: ["—"],
      teams: ["Yes, users and roles", "matrix"],
      access_rules: ["Yes, roles and access requests", "matrix"],
      api_tokens: ["Yes, bots with short-lived credentials", "matrix"],
      selfhost: ["Yes, the whole platform", "matrix"],
      platforms: ["Linux, macOS, Windows clients", "matrix"],
      pricing_model: ["By active users and protected resources", "pricing"],
      free_tier: ["Community Edition for small companies", "matrix"],
    }),
    differences: [
      {
        title: "An access platform, or a tool you install",
        body: [
          "Teleport runs a cluster (auth service and proxy) that every connection goes through. It is built for many servers, many people and compliance.",
          ours.install,
        ],
      },
      {
        title: "Sessions",
        body: [
          "Teleport records sessions and plays them back. People can join a live session, and moderated sessions are Enterprise. A session ends with its connection.",
          ours.terminals,
        ],
        them: "$ tsh ssh dev@devbox\n$ tsh join <session-id>",
        us: "$ pb devbox\n$ pb session share <session>",
      },
      {
        title: "Public links",
        body: [
          "Teleport's application access puts internal apps behind its proxy for signed-in users. It doesn't publish a port to the internet.",
          ours.previews,
        ],
        us: "$ pb preview 3000",
      },
      {
        title: "Audit and compliance",
        body: [
          "Teleport's session recording, access requests and audit export are mature and built for regulated companies.",
          "Paperboat records activity per workspace. Roles, SSO and audit export for businesses are being built. If you need them now, choose Teleport.",
        ],
      },
    ],
    chooseThem: [
      "You need recorded sessions, access requests and audit across many servers.",
      "You manage Kubernetes clusters, databases and Windows desktops for a company.",
    ],
    chooseUs: [
      "You want fast access to your own machines without running an access platform.",
      "You want public previews and tunnels, files and encrypted environment variables too.",
    ],
    pricing: {
      lines: [
        "Community Edition: free for companies under 100 employees and $10M revenue, self-hosted",
        "Enterprise: priced by active users and protected resources",
      ],
      source: teleportSources.pricing,
    },
    switching: [
      install,
      "Use pb ssh or the generated SSH host names instead of tsh ssh for day-to-day access.",
      "Keep Teleport where you need its recording and audit; Paperboat can run beside it.",
    ],
    faq: [
      {
        q: "Does Paperboat record sessions?",
        a: "Terminals keep their scrollback so you can reattach and see what happened; Paperboat doesn't keep recordings for audit.",
      },
      {
        q: "Can I self-host Paperboat like Teleport?",
        a: "You can run your own relays and tunnel servers; the control plane is hosted by Paperboat.",
      },
    ],
  },
  {
    slug: "netbird",
    name: "NetBird",
    label: "NetBird",
    summary:
      "NetBird is an open-source WireGuard network with access policies and a management server you can run yourself. Paperboat also connects your machines privately and adds terminals, SSH, files, secrets, previews and tunnels.",
    cells: cells(netbirdSources, {
      terminal_persistent: ["No", "ssh"],
      terminal_replay: ["No", "ssh"],
      terminal_browser: ["Yes, SSH from the dashboard", "ssh"],
      terminal_share: ["No", "ssh"],
      exec: ["Through ssh", "ssh"],
      ssh_keys: ["Signs in with your identity provider", "ssh"],
      ssh_tools: ["ssh, scp and sftp", "ssh"],
      ssh_vscode: ["As plain SSH over the network", "ssh"],
      files_send: ["No", "docs"],
      files_inbox: ["No", "docs"],
      env_secrets: ["No", "docs"],
      env_where: ["—"],
      config_sync: ["No", "docs"],
      preview_url: ["No", "docs"],
      preview_inspect: ["No", "docs"],
      tunnel_permanent: ["No", "docs"],
      tunnel_domains: ["No", "docs"],
      tunnel_protocols: ["—"],
      private_network: ["Yes, a WireGuard mesh", "docs"],
      encryption_native: ["End to end (WireGuard)", "docs"],
      encryption_public: ["—"],
      teams: ["Yes, users and groups", "plans"],
      access_rules: ["Yes, policies and groups", "ssh"],
      api_tokens: ["Yes, access tokens for its API", "docs"],
      selfhost: ["Yes, the whole service", "docs"],
      platforms: ["Linux, macOS, Windows, iOS, Android", "docs"],
      pricing_model: ["Per active user: €6 or €12 a month", "pricing"],
      free_tier: ["5 users and 100 machines", "pricing"],
    }),
    differences: [
      {
        title: "A network, or a place to work",
        body: [
          "NetBird builds a private WireGuard network with policies. You bring the tools, and its dashboard can open an SSH session in the browser.",
          ours.install,
        ],
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "NetBird SSH sessions end when the connection drops.",
          ours.terminals,
        ],
        them: "$ netbird ssh devbox",
        us: "$ pb devbox",
      },
      {
        title: "Public links",
        body: ["NetBird keeps everything private; it doesn't publish a port to the internet.", ours.previews],
        us: "$ pb preview 3000",
      },
      {
        title: "Running it yourself",
        body: [
          "NetBird is open source. You can run the whole service on your servers.",
          "With Paperboat you run your own relays and tunnel servers, but the control plane is hosted. To self-host everything, choose NetBird.",
        ],
      },
    ],
    chooseThem: [
      "You want a fully open-source network you can host end to end.",
      "You need network access policies across a company.",
    ],
    chooseUs: [
      "You want terminals, previews, public links and file sending in one tool.",
      "You'd rather not run a network control server yourself.",
    ],
    pricing: {
      lines: [
        "Free: 5 users and 100 machines",
        "Team: €6 per user a month",
        "Business: €12 per user a month",
        "Enterprise: by quote",
      ],
      source: netbirdSources.pricing,
    },
    switching: [
      install,
      "Use pb ssh or the generated SSH host names where you used NetBird addresses.",
      "Keep NetBird for subnets and devices Paperboat doesn't run on; they work side by side.",
    ],
    faq: [
      {
        q: "Is Paperboat open source?",
        a: "Paperboat's code is public on GitHub; the hosted control plane is run by Paperboat, and relays and tunnel servers can be self-hosted.",
      },
    ],
  },
  {
    slug: "zerotier",
    name: "ZeroTier",
    label: "ZeroTier",
    summary:
      "ZeroTier makes your devices act like one Ethernet network, with broadcast and bridging. Paperboat connects your machines privately and adds terminals, SSH, files, secrets, previews and tunnels.",
    cells: cells(zerotierSources, {
      terminal_persistent: ["No", "protocol"],
      terminal_replay: ["No", "protocol"],
      terminal_browser: ["No", "protocol"],
      terminal_share: ["No", "protocol"],
      exec: ["No", "protocol"],
      ssh_keys: ["No", "protocol"],
      ssh_tools: ["Your own SSH over the network", "protocol"],
      ssh_vscode: ["Your own SSH over the network", "protocol"],
      files_send: ["No", "protocol"],
      files_inbox: ["No", "protocol"],
      env_secrets: ["No", "protocol"],
      env_where: ["—"],
      config_sync: ["No", "protocol"],
      preview_url: ["No", "protocol"],
      preview_inspect: ["No", "protocol"],
      tunnel_permanent: ["No", "protocol"],
      tunnel_domains: ["No", "protocol"],
      tunnel_protocols: ["—"],
      private_network: ["Yes, a virtual Ethernet network", "protocol"],
      encryption_native: ["End to end; its relays can't read traffic", "protocol"],
      encryption_public: ["—"],
      teams: ["Owners and administrators", "orgs"],
      access_rules: ["Network membership and rules", "orgs"],
      api_tokens: ["Yes, service account tokens", "orgs"],
      selfhost: ["Your own network controller", "controller"],
      platforms: ["Linux, macOS, Windows, iOS, Android and more", "download"],
      pricing_model: ["Per device: from $18 a month", "pricing"],
      free_tier: ["Personal: 10 devices, one network", "pricing"],
    }),
    differences: [
      {
        title: "A virtual LAN, or a place to work",
        body: [
          "ZeroTier emulates Ethernet, so broadcast, multicast and bridging work as on a local network. You bring the other tools.",
          ours.install,
        ],
      },
      {
        title: "Terminals that survive a dropped connection",
        body: ["ZeroTier has no terminal of its own; you use SSH over the network.", ours.terminals],
        us: "$ pb devbox",
      },
      {
        title: "Public links",
        body: ["ZeroTier networks are private; it doesn't publish a port to the internet.", ours.previews],
        us: "$ pb preview 3000",
      },
      {
        title: "Devices that aren't computers",
        body: [
          "ZeroTier runs on routers, NAS boxes and other devices, and can bridge a physical network.",
          "Paperboat runs on Linux and macOS computers (Windows is coming) and connects only those.",
        ],
      },
    ],
    chooseThem: [
      "You need a virtual LAN: broadcast, multicast or bridging, not only IP between computers.",
      "You connect routers, NAS boxes or other devices that run ZeroTier.",
    ],
    chooseUs: [
      "You want terminals, previews, public links and file sending, not only a network.",
      "You'd rather not manage network rules to reach your own machines.",
    ],
    pricing: {
      lines: [
        "Personal: free, 10 devices, one network",
        "Essential: $18 a month, 10 devices, then $2 per device",
        "Scale: $179 a month, 100 devices",
        "Enterprise: by quote, including self-hosted",
      ],
      source: zerotierSources.pricing,
    },
    switching: [
      install,
      "Use pb ssh or the generated SSH host names where you used ZeroTier addresses.",
      "Keep ZeroTier for bridged networks and devices Paperboat doesn't run on.",
    ],
    faq: [
      {
        q: "Does Paperboat do layer 2 networking?",
        a: "No. Your machines reach each other's ports privately by name; there is no broadcast or bridging.",
      },
    ],
  },
  {
    slug: "twingate",
    name: "Twingate",
    label: "Twingate",
    summary:
      "Twingate gives a company's people access to private resources without a VPN, using connectors you deploy and policies tied to your identity provider. Paperboat is for working on machines: terminals, SSH, files, secrets, previews and tunnels.",
    cells: cells(twingateSources, {
      terminal_persistent: ["No", "ssh"],
      terminal_replay: ["No", "ssh"],
      terminal_browser: ["No", "ssh"],
      terminal_share: ["No", "ssh"],
      exec: ["Through SSH", "ssh"],
      ssh_keys: ["Yes, short-lived certificates (Privileged Access)", "ssh"],
      ssh_tools: ["Yes, ssh, sftp and rsync", "ssh"],
      ssh_vscode: ["As SSH to a resource", "ssh"],
      files_send: ["Through sftp or rsync", "ssh"],
      files_inbox: ["No", "ssh"],
      env_secrets: ["No", "faq"],
      env_where: ["—"],
      config_sync: ["No", "faq"],
      preview_url: ["No", "faq"],
      preview_inspect: ["No", "faq"],
      tunnel_permanent: ["No, private resources only", "resources"],
      tunnel_domains: ["Private names only", "resources"],
      tunnel_protocols: ["TCP, UDP and ICMP, privately", "resources"],
      private_network: ["People reach resources, not machines each other", "resources"],
      encryption_native: ["End to end, client to connector", "relays"],
      encryption_public: ["—"],
      teams: ["Yes, users and admin roles", "users"],
      access_rules: ["Yes, groups, policies and approvals", "users"],
      api_tokens: ["Yes, API tokens", "api"],
      selfhost: ["Connectors only", "connectors"],
      platforms: ["Linux, macOS, Windows, iOS, Android, ChromeOS", "clients"],
      pricing_model: ["Per user: $5 or $10 a month", "pricing"],
      free_tier: ["Starter: 5 users", "pricing"],
    }),
    differences: [
      {
        title: "People to resources, or machines to each other",
        body: [
          "Twingate connects users' devices to private resources through connectors. It is built to give employees access.",
          "Paperboat connects the machines you install it on and lets you work on them from anywhere.",
        ],
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "Twingate's Privileged Access for SSH signs connections with short-lived certificates. The session ends with the connection.",
          ours.terminals,
        ],
        us: "$ pb devbox",
      },
      {
        title: "Public links",
        body: ["Twingate only makes resources reachable privately.", ours.previews],
        us: "$ pb preview 3000",
      },
      {
        title: "Files, secrets and settings",
        body: ["Twingate moves files over SFTP or rsync and has no environment variables or config sync.", ours.files],
        us: "$ pb send report.pdf --to laptop",
      },
    ],
    chooseThem: [
      "You run a company network and need people to reach internal resources with device checks.",
      "You want access policies tied to your identity provider for many users.",
    ],
    chooseUs: [
      "You want to work on your own machines from anywhere, with terminals and files.",
      "You want public previews and permanent tunnels.",
    ],
    pricing: {
      lines: [
        "Starter: free, up to 5 users",
        "Teams: $5 per user a month (yearly)",
        "Business: $10 per user a month (yearly)",
        "Enterprise: by quote",
      ],
      source: twingateSources.pricing,
    },
    switching: [
      install,
      "Use Paperboat for access to your own machines; keep Twingate for company resources.",
    ],
    faq: [
      {
        q: "Does Paperboat check devices before giving access?",
        a: "Access is per signed-in account and machine; device posture checks are not part of Paperboat.",
      },
    ],
  },
  {
    slug: "cloudflare-zero-trust",
    name: "Cloudflare Zero Trust",
    label: "Cloudflare Zero Trust",
    summary:
      "Cloudflare Zero Trust (Access, WARP and Tunnel) puts a company's apps and networks behind Cloudflare, with identity policies and web filtering. Paperboat is for working on your own machines, end to end encrypted, without moving your DNS.",
    cells: cells(cfZeroTrustSources, {
      terminal_persistent: ["No", "setup"],
      terminal_replay: ["No", "setup"],
      terminal_browser: ["Yes, browser SSH and RDP", "setup"],
      terminal_share: ["No", "setup"],
      exec: ["Through SSH", "ssh"],
      ssh_keys: ["Yes, short-lived certificates", "ssh"],
      ssh_tools: ["Yes, through WARP or cloudflared", "ssh"],
      ssh_vscode: ["As SSH through WARP", "ssh"],
      files_send: ["No", "setup"],
      files_inbox: ["No", "setup"],
      env_secrets: ["No", "setup"],
      env_where: ["—"],
      config_sync: ["No", "setup"],
      preview_url: ["Through Tunnel, set up in the dashboard", "tunnel"],
      preview_inspect: ["No", "tunnel"],
      tunnel_permanent: ["Yes, named tunnels", "tunnel"],
      tunnel_domains: ["Yes, on a domain using Cloudflare DNS", "routing"],
      tunnel_protocols: ["HTTP, HTTPS, TCP, SSH, RDP and more", "routing"],
      private_network: ["Yes, routed through Cloudflare", "private"],
      encryption_native: ["Through Cloudflare, which can inspect it", "private"],
      encryption_public: ["Ends at Cloudflare", "tunnel"],
      teams: ["Yes, seats in your organization", "users"],
      access_rules: ["Yes, identity and device policies", "policies"],
      api_tokens: ["Yes, Cloudflare API tokens", "api"],
      selfhost: ["The connector only", "tunnel"],
      platforms: ["Linux, macOS, Windows, iOS, Android, ChromeOS", "warp"],
      pricing_model: ["Per user: $7 a month", "pricing"],
      free_tier: ["Up to 50 users", "limits"],
    }),
    differences: [
      {
        title: "A company's perimeter, or your machines",
        body: [
          "Cloudflare Zero Trust secures a company: who reaches which app, from which device, with web filtering and logs.",
          ours.install,
        ],
      },
      {
        title: "Who can read the traffic",
        body: [
          "With WARP, your traffic goes through Cloudflare, which applies policies and can inspect it.",
          "Paperboat encrypts end to end between your devices and connects them directly when it can. Relays forward traffic they can't read. Only public links end TLS at Paperboat's edge.",
        ],
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "Access can show SSH in the browser. The session ends with the tab or connection.",
          ours.terminals,
        ],
        us: "$ pb devbox",
      },
      {
        title: "Your DNS stays where it is",
        body: [
          "Public hostnames on Cloudflare need the domain's DNS on Cloudflare.",
          "Paperboat asks for two CNAME records at your current DNS provider.",
        ],
        us: "$ pb tunnel domain add api api.example.com",
      },
    ],
    chooseThem: [
      "You secure a whole company: web filtering, device posture and app access in one place.",
      "Your domains are already on Cloudflare.",
    ],
    chooseUs: [
      "You want terminals that survive disconnects, previews and file sending on your own machines.",
      "You want your machines to talk to each other directly, encrypted end to end.",
    ],
    pricing: {
      lines: ["Free: up to 50 users", "Pay-as-you-go: $7 per user a month", "Contract plans: by quote"],
      source: cfZeroTrustSources.pricing,
    },
    switching: [
      install,
      "Use Paperboat for your own machines; keep Zero Trust for company apps and filtering.",
    ],
    faq: [
      {
        q: "Can Paperboat replace a company VPN?",
        a: "Not as a whole: it connects the machines it is installed on, not whole networks, and has no web filtering.",
      },
    ],
  },
  {
    slug: "enterprise-vpn",
    name: "an enterprise VPN",
    label: "Enterprise VPN",
    summary:
      "Zscaler Private Access, Palo Alto Prisma Access, Cisco Secure Client (AnyConnect) and OpenVPN Access Server connect employees to a company network with central policy. Paperboat connects you to your own machines.",
    cells: cells(vpnSources, {
      terminal_persistent: ["No", "zpa"],
      terminal_replay: ["No", "zpa"],
      terminal_browser: ["Some, as browser access to apps", "zpa"],
      terminal_share: ["No", "zpa"],
      exec: ["No; SSH once connected", "openvpn"],
      ssh_keys: ["No", "openvpn"],
      ssh_tools: ["Your own SSH once connected", "openvpn"],
      ssh_vscode: ["Your own SSH once connected", "openvpn"],
      files_send: ["No", "openvpn"],
      files_inbox: ["No", "openvpn"],
      env_secrets: ["No", "openvpn"],
      env_where: ["—"],
      config_sync: ["No", "openvpn"],
      preview_url: ["No", "openvpn"],
      preview_inspect: ["No", "openvpn"],
      tunnel_permanent: ["No", "openvpn"],
      tunnel_domains: ["No", "openvpn"],
      tunnel_protocols: ["—"],
      private_network: ["Into the company network", "prisma"],
      encryption_native: ["To the gateway or the vendor's cloud", "zpa"],
      encryption_public: ["—"],
      teams: ["Through your identity provider", "zpa"],
      access_rules: ["Yes, detailed central policy", "prisma"],
      api_tokens: ["Varies by vendor", "zpa"],
      selfhost: ["OpenVPN AS and Cisco gateways; Zscaler and Prisma are cloud", "openvpn"],
      platforms: ["Desktop and mobile clients", "cisco"],
      pricing_model: ["By quote; OpenVPN AS per connection", "openvpnPricing"],
      free_tier: ["OpenVPN AS: 2 connections", "openvpnPricing"],
    }),
    differences: [
      {
        title: "A company network, or your machines",
        body: [
          "An enterprise VPN or ZTNA product puts employees inside the company network, with policy run by IT.",
          "Paperboat is for the machines you work on: a laptop, a server, a box under a desk. It doesn't route a network.",
        ],
      },
      {
        title: "Nothing to run",
        body: [
          "These products need gateways, connectors or a cloud tenant that someone sets up and maintains.",
          ours.install,
        ],
      },
      {
        title: "Working on the machine",
        body: ["Once connected to a VPN, you still bring your own SSH, tmux and file copying.", ours.terminals],
        us: "$ pb devbox",
      },
      {
        title: "Public links",
        body: ["VPNs keep things private by design.", ours.previews],
        us: "$ pb preview 3000",
      },
    ],
    chooseThem: [
      "Your company needs every employee on the corporate network with central policy and compliance.",
      "Your security team already runs one of these.",
    ],
    chooseUs: [
      "You're a developer or small team who wants your own machines, not a corporate network.",
      "You want to be working in a minute, without a gateway to run.",
    ],
    pricing: {
      lines: [
        "Zscaler, Prisma Access, Cisco: by quote",
        "OpenVPN Access Server: free for 2 connections, then $7 per connection a month",
      ],
      source: vpnSources.openvpnPricing,
    },
    switching: [
      install,
      "Use Paperboat for your own development machines; keep the VPN for company systems that require it.",
    ],
    faq: [
      {
        q: "Can I use Paperboat while connected to a company VPN?",
        a: "Usually yes: Paperboat makes outbound connections and uses relays when direct paths are blocked.",
      },
    ],
  },
  {
    slug: "ssh-bastion",
    name: "SSH and a bastion",
    label: "SSH + bastion",
    summary:
      "OpenSSH through a bastion (jump) host, with tmux to keep work running, is the classic setup. Paperboat keeps SSH and adds terminals that survive disconnects, files, secrets, previews and tunnels, with no open port.",
    cells: cells(sshSources, {
      terminal_persistent: ["With tmux or screen", "tmux"],
      terminal_replay: ["With tmux scrollback", "tmux"],
      terminal_browser: ["No", "ssh"],
      terminal_share: ["With a shared tmux session", "tmux"],
      exec: ["Yes, ssh host command", "ssh"],
      ssh_keys: ["You manage keys on every host", "sshd"],
      ssh_tools: ["Yes, the original", "scp"],
      ssh_vscode: ["Yes, with ProxyJump", "config"],
      files_send: ["scp or rsync", "scp"],
      files_inbox: ["No", "scp"],
      env_secrets: ["No; SetEnv sends plain variables", "config"],
      env_where: ["—"],
      config_sync: ["No", "openssh"],
      preview_url: ["Only with ssh -R to a server you run", "ssh"],
      preview_inspect: ["No", "ssh"],
      tunnel_permanent: ["On servers you run", "ssh"],
      tunnel_domains: ["On servers you run", "ssh"],
      tunnel_protocols: ["TCP forwarding", "ssh"],
      private_network: ["Port forwarding through the bastion", "ssh"],
      encryption_native: ["End to end through the jump host", "ssh"],
      encryption_public: ["Whatever you set up", "ssh"],
      teams: ["Unix users on each host", "sshd"],
      access_rules: ["sshd_config per host", "sshd"],
      api_tokens: ["No", "sshd"],
      selfhost: ["All of it, and you maintain it", "openssh"],
      platforms: ["Everywhere OpenSSH runs", "openssh"],
      pricing_model: ["Free software; you pay for the bastion", "openssh"],
      free_tier: ["Free", "openssh"],
    }),
    differences: [
      {
        title: "No open port, no bastion to patch",
        body: [
          "A bastion needs a public address, an open port 22, and someone to patch it and manage keys.",
          "Paperboat machines make only outbound connections and reach each other directly or through relays. Nothing is exposed.",
        ],
        them: "$ ssh -J bastion.example.com dev@10.0.1.5",
        us: "$ pb ssh devbox",
      },
      {
        title: "Persistence built in",
        body: [
          "With SSH you add tmux or screen on every host to keep work running and share it.",
          ours.terminals,
        ],
        them: "$ ssh devbox -t tmux new -A -s work",
        us: "$ pb devbox",
      },
      {
        title: "Your tools keep working",
        body: [
          "Paperboat generates SSH host names. Plain ssh, scp, rsync, VS Code Remote-SSH and anything built on OpenSSH work as before, with no keys to copy.",
        ],
        us: "$ ssh devbox.{localDomain}\n$ rsync -a ./site devbox.{localDomain}:site",
      },
      {
        title: "Public links",
        body: ["Reverse forwarding (ssh -R) needs a public server you run.", ours.previews],
        them: "$ ssh -R 80:localhost:3000 server.example.com",
        us: "$ pb preview 3000",
      },
    ],
    chooseThem: [
      "You already run servers with public addresses and keys you manage well.",
      "You want nothing but OpenSSH.",
    ],
    chooseUs: [
      "Your machines sit behind NAT or at home, with no open port.",
      "You want keys, sharing and previews handled for you, and terminals that come back after a dropped connection.",
    ],
    pricing: {
      lines: ["OpenSSH and tmux: free software", "You pay for and maintain the bastion"],
      source: sshSources.openssh,
    },
    switching: [
      install,
      "Point your SSH tools at the generated host names instead of the bastion.",
      "Close the bastion's public port once nothing uses it.",
    ],
    faq: [
      {
        q: "Does Paperboat replace OpenSSH?",
        a: "No. It uses it: pb ssh and the generated host names run your normal ssh with keys and host trust handled.",
      },
    ],
  },
  {
    slug: "vscode-tunnels",
    name: "VS Code Remote Tunnels",
    label: "VS Code Tunnels",
    summary:
      "VS Code Remote Tunnels opens a remote machine in VS Code or vscode.dev through Microsoft's dev tunnels, and can forward ports, also publicly. Paperboat works with any terminal, editor or browser and adds persistent terminals, files, secrets, permanent tunnels and sharing.",
    cells: cells(vscodeSources, {
      terminal_persistent: ["No; only while VS Code runs there", "tunnels"],
      terminal_replay: ["No", "tunnels"],
      terminal_browser: ["Yes, in vscode.dev", "tunnels"],
      terminal_share: ["No, one user at a time", "tunnels"],
      exec: ["No", "tunnels"],
      ssh_keys: ["Signs in with GitHub or Microsoft", "tunnels"],
      ssh_tools: ["No; Remote-SSH is separate", "overview"],
      ssh_vscode: ["It is VS Code's own alternative", "overview"],
      files_send: ["No", "tunnels"],
      files_inbox: ["No", "tunnels"],
      env_secrets: ["No", "tunnels"],
      env_where: ["—"],
      config_sync: ["VS Code settings sync only", "overview"],
      preview_url: ["Yes, a forwarded port made public", "ports"],
      preview_inspect: ["In browser DevTools; no replay", "devtunnels"],
      tunnel_permanent: ["Yes, persistent dev tunnels", "cli"],
      tunnel_domains: ["No", "cli"],
      tunnel_protocols: ["HTTP, HTTPS, WebSocket", "cli"],
      private_network: ["No", "tunnels"],
      encryption_native: ["End to end (SSH inside the tunnel)", "tunnels"],
      encryption_public: ["TLS ends at Microsoft's service", "security"],
      teams: ["Share with a tenant or GitHub org", "cli"],
      access_rules: ["Per tunnel and port", "cli"],
      api_tokens: ["Yes, tunnel tokens (24 hours)", "security"],
      selfhost: ["No; the relay runs on Azure", "repo"],
      platforms: ["Where VS Code Server runs", "tunnels"],
      pricing_model: ["Free, with usage limits", "limits"],
      free_tier: ["10 tunnels, 5 GB a month per user", "limits"],
    }),
    differences: [
      {
        title: "One editor, or any tool",
        body: [
          "Remote Tunnels is built for VS Code. The machine opens in VS Code or vscode.dev, signed in with the same GitHub or Microsoft account on both ends.",
          "Paperboat works with any terminal, editor or agent, including VS Code over Remote-SSH.",
        ],
        them: "$ code tunnel",
        us: "$ pb devbox\n$ ssh devbox.{localDomain}",
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "The machine is reachable while VS Code runs there, for one person at a time.",
          ours.terminals,
        ],
      },
      {
        title: "Public links and domains",
        body: [
          "A forwarded port can be public, and dev tunnels can keep a stable address on Microsoft's domain. Your own domains aren't supported.",
          ours.previews,
        ],
        them: "$ devtunnel host -p 3000 --allow-anonymous",
        us: "$ pb preview 3000",
      },
      {
        title: "Files, secrets and settings",
        body: ["VS Code syncs its own settings; it has no file sending or encrypted environment variables.", ours.files],
      },
    ],
    chooseThem: [
      "You only want to open a remote folder in VS Code.",
      "You already sign in to VS Code with GitHub and need nothing else.",
    ],
    chooseUs: [
      "You use other editors, plain terminals, agents or a phone.",
      "You want permanent tunnels, your own domains, sharing with a team, or plain SSH.",
    ],
    pricing: {
      lines: ["Free with a GitHub or Microsoft account", "Dev tunnels limits: 10 tunnels and 5 GB a month per user"],
      source: vscodeSources.limits,
    },
    switching: [
      install,
      "Connect VS Code with Remote-SSH to the generated host name.",
      "Move public ports to pb preview or a tunnel.",
    ],
    faq: [
      {
        q: "Does VS Code Remote-SSH work with Paperboat?",
        a: "Yes. Paperboat generates SSH host names; pick one in Remote-SSH and it connects with keys handled.",
      },
    ],
  },
]

/** The default comparison on /compare (?vs= overrides it). */
export const defaultPicks = ["tailscale", "ngrok"]

/** The nav and footer list the long-form pages. */
export const compares = products
