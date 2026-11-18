/**
 * The compare pages: /compare (pick up to two products and see them beside Paperboat) and one
 * long page per product (pages/compare/[slug].astro). Every claim about another product links to
 * the page of its own documentation, pricing page or repository that says it; claims are rechecked
 * before each release of the site, and `checked` is the month of the last pass.
 * "No" means the product's documentation describes no such feature.
 */

/** When the claims about other products were last checked against their sources. */
export const checked = "November 2026"

export type RowId =
  | "terminal_persistent"
  | "terminal_replay"
  | "terminal_browser"
  | "terminal_share"
  | "terminal_record"
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
  | "private_names"
  | "setup_one_line"
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

/** What every comparison answers, grouped the way Paperboat's features are. */
export const sections: Section[] = [
  {
    id: "terminals",
    title: "Terminals",
    sentence: "A shell on your machine that you can leave and come back to.",
    rows: [
      { id: "terminal_persistent", label: "Keeps running when you disconnect" },
      { id: "terminal_replay", label: "Shows the output you missed" },
      { id: "terminal_browser", label: "From a browser" },
      { id: "terminal_share", label: "Share a live session" },
      { id: "terminal_record", label: "Record and replay sessions" },
    ],
  },
  {
    id: "exec",
    title: "Remote commands",
    sentence: "Run one command on another machine and get its output and exit code.",
    rows: [{ id: "exec", label: "Run a command on another machine" }],
  },
  {
    id: "ssh",
    title: "SSH",
    sentence: "Use ssh and the tools built on it, without managing keys or opening ports.",
    rows: [
      { id: "ssh_keys", label: "Keys handled for you" },
      { id: "ssh_tools", label: "ssh, scp, sftp and rsync" },
      { id: "ssh_vscode", label: "VS Code Remote-SSH" },
    ],
  },
  {
    id: "files",
    title: "Files",
    sentence: "Send a file to a machine without setting anything up.",
    rows: [
      { id: "files_send", label: "Send files to a machine" },
      { id: "files_inbox", label: "An inbox for received files" },
    ],
  },
  {
    id: "env",
    title: "Environment variables",
    sentence: "The secrets your programs need, delivered to the machine that runs them.",
    rows: [
      { id: "env_secrets", label: "End-to-end encrypted variables" },
      { id: "env_where", label: "Where they are available" },
    ],
  },
  {
    id: "config",
    title: "Config sync",
    sentence: "The same settings on every machine, from a repository you control.",
    rows: [{ id: "config_sync", label: "Settings from a Git repository" }],
  },
  {
    id: "previews",
    title: "Previews",
    sentence: "A public link to a port on your machine, for as long as you need it.",
    rows: [
      { id: "preview_url", label: "Public link in one command" },
      { id: "preview_inspect", label: "See and replay requests" },
    ],
  },
  {
    id: "tunnels",
    title: "Tunnels",
    sentence: "A public address for a service that stays the same.",
    rows: [
      { id: "tunnel_permanent", label: "Permanent addresses" },
      { id: "tunnel_domains", label: "Your own domains" },
      { id: "tunnel_protocols", label: "Protocols" },
    ],
  },
  {
    id: "network",
    title: "Private network",
    sentence: "Your machines reach each other's ports without exposing them.",
    rows: [
      { id: "private_network", label: "Machines reach each other" },
      { id: "private_names", label: "A private HTTPS address for each port" },
    ],
  },
  {
    id: "setup",
    title: "New machines",
    sentence: "Bring a new machine in, set up and ready.",
    rows: [{ id: "setup_one_line", label: "Set up any number of machines in one command" }],
  },
  {
    id: "encryption",
    title: "Encryption",
    sentence: "Who could read your traffic on the way.",
    rows: [
      { id: "encryption_native", label: "Between your own devices" },
      { id: "encryption_public", label: "Public links" },
    ],
  },
  {
    id: "teams",
    title: "Teams and access",
    sentence: "Working with other people, and deciding who may do what.",
    rows: [
      { id: "teams", label: "Members and roles" },
      { id: "access_rules", label: "Access rules, groups and tags" },
      { id: "api_tokens", label: "Tokens for automation" },
    ],
  },
  {
    id: "selfhost",
    title: "Self-hosting",
    sentence: "Which parts you can run on your own servers.",
    rows: [{ id: "selfhost", label: "Run it yourself" }],
  },
  {
    id: "platforms",
    title: "Platforms",
    sentence: "Where it runs.",
    rows: [{ id: "platforms", label: "Operating systems" }],
  },
  {
    id: "pricing",
    title: "Pricing",
    sentence: "How you pay, and what is free.",
    rows: [
      { id: "pricing_model", label: "How you pay" },
      { id: "free_tier", label: "Free plan" },
    ],
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

/** Paperboat's own answers: what is verified on Linux and macOS (CHECKLIST.md). */
export const paperboat = {
  name: "Paperboat",
  cells: cells({}, {
    terminal_persistent: ["Yes, sessions keep running on the machine"],
    terminal_replay: ["Yes, replays what you missed on reconnect"],
    terminal_browser: ["Yes, in the web console"],
    terminal_share: ["Yes, view-only or interactive, by invite"],
    terminal_record: ["Yes, on every plan, encrypted on the machine"],
    exec: ["Yes, pb exec, with exit codes"],
    ssh_keys: ["Yes, keys and host trust handled"],
    ssh_tools: ["Yes, pb ssh, scp, sftp, rsync and plain ssh"],
    ssh_vscode: ["Yes, through generated SSH host names"],
    files_send: ["Yes, pb send, resumes if interrupted"],
    files_inbox: ["Yes, an Inbox on each machine"],
    env_secrets: ["Yes, end-to-end encrypted"],
    env_where: ["Terminals, pb exec, scripts, plain SSH and VS Code"],
    config_sync: ["Yes, from your Git repository"],
    preview_url: ["Yes, pb preview 3000"],
    preview_inspect: ["Yes, inspect and replay, in pb and the console"],
    tunnel_permanent: ["Yes, names that stay"],
    tunnel_domains: ["Yes, on every plan"],
    tunnel_protocols: ["HTTP/1.1, HTTP/2, HTTP/3, WebSocket, raw TCP"],
    private_network: ["Yes, encrypted end to end"],
    private_names: ["Yes, by port number or a name you choose"],
    setup_one_line: ["Yes, recipes: access, secrets, config, previews"],
    encryption_native: ["End to end; direct when possible"],
    encryption_public: ["TLS ends at Paperboat's edge"],
    teams: ["Yes, five roles; SSO and SCIM from 10 seats"],
    access_rules: ["Yes, a policy file with groups, tags and deny rules"],
    api_tokens: ["Yes, scoped API tokens that expire"],
    selfhost: ["Relays and tunnel servers"],
    platforms: ["Linux, macOS and Windows"],
    pricing_model: ["By bandwidth through our servers; Team per seat"],
    free_tier: ["Every feature, 5 GB/mo, never cut off"],
  }),
  pricing: [
    "Free: $0, every feature for personal use, 5 GB/mo, then 512 kbit/s",
    "Pro: $8 a month or $80 a year, 100 GB/mo, then 1 Mbit/s",
    "Team: $15 per seat a month or $150 a year, 100 GB/mo per seat; 14-day trial",
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
  /** One line on what moving to Paperboat gets you, for lists of comparisons. */
  tagline: string
  /** The page's h1: what you get, in a few words. */
  headline: string
  summary: string
  /** What Paperboat gives you that you'd want coming from this product, a few words each. */
  gains: string[]
  cells: Cells
  differences: Difference[]
  pricing: { lines: string[]; source: string }
  switching: string[]
  faq: { q: string; a: string }[]
}

const install = "Install Paperboat on each machine with the one-line installer and sign in."

// Every price and limit below was re-opened on the vendor's own page on 2026-11-10; a number that
// cannot be opened there is removed, not kept from memory.
// Tailscale
const tailscaleSources = {
  ssh: "https://tailscale.com/kb/1193/tailscale-ssh",
  console: "https://tailscale.com/kb/1216/tailscale-ssh-console",
  funnel: "https://tailscale.com/kb/1223/funnel",
  taildrop: "https://tailscale.com/kb/1106/taildrop",
  policy: "https://tailscale.com/kb/1337/policy-syntax",
  keys: "https://tailscale.com/kb/1085/auth-keys",
  derp: "https://tailscale.com/kb/1118/custom-derp-servers",
  recorder: "https://tailscale.com/docs/features/tsrecorder",
  serve: "https://tailscale.com/kb/1312/serve",
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
  join: "https://goteleport.com/docs/reference/join-methods/",
  pricing: "https://goteleport.com/pricing/",
}

// NetBird
const netbirdSources = {
  docs: "https://docs.netbird.io/",
  ssh: "https://docs.netbird.io/manage/peers/ssh",
  setupKeys: "https://docs.netbird.io/manage/peers/register-machines-using-setup-keys",
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
  recording: "https://www.twingate.com/docs/ssh-session-recording-compliance",
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
  sshLogs: "https://developers.cloudflare.com/cloudflare-one/insights/logs/dashboard-logs/ssh-command-logs/",
  tunnel: "https://developers.cloudflare.com/tunnel/",
  routing: "https://developers.cloudflare.com/tunnel/routing/",
  private: "https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/private-net/",
  policies: "https://developers.cloudflare.com/cloudflare-one/policies/access/",
  users: "https://developers.cloudflare.com/cloudflare-one/team-and-resources/users/seat-management/",
  api: "https://developers.cloudflare.com/api/",
  warp: "https://developers.cloudflare.com/cloudflare-one/connections/connect-devices/warp/",
  // The page that states the numbers as text (the HTML pricing page draws them with script).
  pricing: "https://www.cloudflare.com/plans/sase-zero-trust.md",
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
  openssh: "https://www.openssh.org/",
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
  extras:
    "Paperboat records its terminals on every plan, encrypted on the machine, and plays them back in the console or your terminal. A recipe sets up one machine or hundreds with one command, with their access, secrets, config and previews, reachable from the start.",
  terminals:
    "Paperboat terminals run on the machine and keep running when your laptop sleeps or the network drops. When you come back, from the same computer, another one or a browser, you see what happened while you were away, and you can let someone watch or type along.",
  previews:
    "pb preview gives a port a public HTTPS link in one command, and pb tunnel keeps a permanent address with your own domain, on every plan.",
  files:
    "pb send puts files in the other machine's Inbox, resuming if the connection drops. ENV delivers encrypted variables to the commands and terminals that need them, and config sync keeps settings the same everywhere from a Git repository.",
  install:
    "Paperboat is one install per machine and a sign-in. There is no network, gateway or proxy to plan, and nothing to keep running besides the machines themselves.",
}

export const products: Product[] = [
  {
    slug: "tailscale",
    name: "Tailscale",
    label: "Tailscale",
    tagline: "Reach your machines, then work on them: terminals, files, secrets and previews.",
    headline: "Reach your machines, then get to work",
    summary:
      "Tailscale connects your devices into one private network. Paperboat connects your machines too, and gives you what you do once you're there: terminals that keep running, an HTTPS address for every port, files, secrets and public links.",
    gains: [
      "Terminals that keep running when you disconnect",
      "An HTTPS address for every port, by name",
      "Public previews and tunnels on your own domain",
      "Encrypted secrets and config on every machine",
      "Session recordings on every plan",
    ],
    cells: cells(tailscaleSources, {
      terminal_persistent: ["No, a standard SSH session", "ssh"],
      terminal_replay: ["No", "ssh"],
      terminal_browser: ["Yes, SSH Console in the admin console (beta)", "console"],
      terminal_share: ["No", "ssh"],
      terminal_record: ["Through Tailscale PAM; its older recorder is deprecated", "recorder"],
      exec: ["Through ssh", "ssh"],
      ssh_keys: ["Yes, with Tailscale SSH", "ssh"],
      ssh_tools: ["Yes, over the private network", "ssh"],
      ssh_vscode: ["Yes, as plain SSH over the network", "ssh"],
      files_send: ["Yes, Taildrop between your own devices", "taildrop"],
      files_inbox: ["No; files land in Downloads", "taildrop"],
      env_secrets: ["No", "pricing"],
      env_where: ["n/a"],
      config_sync: ["No", "pricing"],
      preview_url: ["Yes, tailscale funnel 3000", "funnel"],
      preview_inspect: ["No", "funnel"],
      tunnel_permanent: ["Yes, on your tailnet's ts.net name", "funnel"],
      tunnel_domains: ["No, ts.net names only", "funnel"],
      tunnel_protocols: ["HTTPS on ports 443, 8443, 10000", "funnel"],
      private_network: ["Yes, a WireGuard mesh", "pricing"],
      private_names: ["One HTTPS name per device, through Serve", "serve"],
      setup_one_line: ["Joins with an auth key and tags; the rest is yours", "keys"],
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
          "Tailscale gives every device an address on a private network and leaves the rest to the tools you already use: you open ssh, start tmux, copy files with scp.",
          ours.install,
        ],
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "A Tailscale SSH session is a normal SSH session: when the connection drops, the shell ends unless you started tmux. SSH Console opens a shell in the admin console's browser tab, for owners and admins, and also ends when the tab closes.",
          ours.terminals,
        ],
        them: "$ ssh devbox\n$ tmux new -A -s work",
        us: "$ pb devbox\n# later, from any device\n$ pb connect devbox",
      },
      {
        title: "Public links",
        body: [
          "Funnel shares a local HTTPS service on your tailnet's ts.net name, on ports 443, 8443 or 10000. Your own domains aren't supported for Funnel.",
          ours.previews,
        ],
        them: "$ tailscale funnel 3000",
        us: "$ pb preview 3000\n$ pb tunnel create api --port 3000",
      },
      {
        title: "Files, secrets and settings",
        body: [
          "Taildrop sends files between your own devices, into Downloads. Tailscale has no environment variables or config sync.",
          ours.files,
        ],
        them: "$ tailscale file cp report.pdf laptop:",
        us: "$ pb send report.pdf --to laptop",
      },
      {
        title: "Access per person, per machine",
        body: [
          "Tailscale's policy file (grants, ACLs, groups, tags) decides which devices can reach which addresses on the network.",
          "Paperboat's policy file decides what each person may do on each machine: open a terminal, run a command, send a file, reach a port. One command explains any decision and names the rule.",
        ],
      },
      {
        title: "Recordings and new machines",
        body: [
          "Tailscale records SSH sessions through Tailscale PAM, and new devices join with an auth key. What runs on them after that is up to you.",
          ours.extras,
        ],
        us: "$ pb recordings start\n$ pb recipe create -f ci.toml",
      },
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
        a: "No, and you don't need one. Each machine gets a private name and each port an HTTPS address, with no network adapter, so it runs next to any VPN.",
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
    tagline: "Public links like ngrok, plus terminals, SSH and files on the same machines.",
    headline: "More than a link to localhost",
    summary:
      "ngrok puts a local service on a public URL. Paperboat does that too, with permanent tunnels and your own domains on every plan, and it gives you the machine itself: terminals, SSH, files and secrets.",
    gains: [
      "Previews and permanent tunnels on every plan",
      "Your own domains with certificates, free",
      "A request inspector with replay",
      "Terminals, SSH and files on the same machines",
    ],
    cells: cells(ngrokSources, {
      terminal_persistent: ["No", "agent"],
      terminal_replay: ["No", "agent"],
      terminal_browser: ["No", "agent"],
      terminal_share: ["No", "agent"],
      terminal_record: ["No", "agent"],
      exec: ["No", "agent"],
      ssh_keys: ["No", "agent"],
      ssh_tools: ["Only as a TCP endpoint", "agent"],
      ssh_vscode: ["No", "agent"],
      files_send: ["No", "agent"],
      files_inbox: ["No", "agent"],
      env_secrets: ["No", "agent"],
      env_where: ["n/a"],
      config_sync: ["No", "agent"],
      preview_url: ["Yes, ngrok http 3000", "start"],
      preview_inspect: ["Yes, traffic inspector with replay", "start"],
      tunnel_permanent: ["Yes, a fixed dev domain; more on paid plans", "domains"],
      tunnel_domains: ["Yes, from Pay-as-you-go", "pricing"],
      tunnel_protocols: ["HTTP, HTTPS, TCP, TLS", "pricing"],
      private_network: ["No", "agent"],
      private_names: ["n/a"],
      setup_one_line: ["n/a"],
      encryption_native: ["n/a"],
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
          "ngrok is built for one thing: getting traffic to a service. It doesn't give you a shell, files or secrets on the machine behind it.",
          "Paperboat gives you the link and the machine: open a terminal on it, copy files to it, run a command, from your laptop or a browser.",
        ],
        them: "$ ngrok http 3000",
        us: "$ pb preview 3000",
      },
      {
        title: "Your own domains",
        body: [
          "On ngrok, bringing your own domain starts at the Pay-as-you-go plan; Free has one development domain and Hobbyist ngrok-branded ones.",
          "Paperboat tunnels take your own domains on every plan, including Free, with the DNS records shown for you and the certificate issued automatically.",
        ],
        them: "$ ngrok http 3000 --url api.example.com",
        us: "$ pb tunnel route add api --name web \\\n    --to http://localhost:3000\n$ pb tunnel domain add api api.example.com \\\n    --route web",
      },
      {
        title: "Request inspection",
        body: [
          "ngrok's traffic inspector shows each request and lets you replay it; that is one of its strongest features.",
          "Paperboat records requests on your own machine once you turn capture on, and lists and replays them from pb or the console, on every plan. ngrok's inspector is always on and keeps traffic for 24 to 72 hours depending on the plan, which is handier when you did not know you would need it.",
        ],
      },
      {
        title: "How you pay",
        body: [
          "ngrok plans add charges per user, per active domain hour and for data and requests beyond what is included.",
          "Paperboat plans differ only in bandwidth through its edges and relays, and nothing is ever cut off. Previews, tunnels and domains have no separate charges, and direct traffic between your machines is free.",
        ],
      },
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
        a: "Yes. A tunnel can have a tcp route with its own public port, and a tls route that passes your app's own TLS through.",
      },
    ],
  },
  {
    slug: "cloudflare-tunnel",
    name: "Cloudflare Tunnel",
    label: "Cloudflare Tunnel",
    tagline: "Put a service online on your own domain, with no DNS to move.",
    headline: "Tunnels without moving your DNS",
    summary:
      "Cloudflare Tunnel connects a service to Cloudflare's network, on a domain whose DNS Cloudflare runs. Paperboat gives you tunnels and previews on any domain, wherever its DNS lives, plus terminals, SSH, files and secrets on the same machines.",
    gains: [
      "Keep your DNS where it is",
      "Quick previews and permanent tunnels in one tool",
      "Terminals, SSH and files on the same machines",
      "Your own tunnel server, if you want one",
    ],
    cells: cells(cfTunnelSources, {
      terminal_persistent: ["No", "tunnel"],
      terminal_replay: ["No", "tunnel"],
      terminal_browser: ["With Cloudflare Access", "private"],
      terminal_share: ["No", "tunnel"],
      terminal_record: ["No", "tunnel"],
      exec: ["No", "tunnel"],
      ssh_keys: ["With Cloudflare Access", "private"],
      ssh_tools: ["Yes, with cloudflared on the client", "setup"],
      ssh_vscode: ["With cloudflared as a proxy", "setup"],
      files_send: ["No", "tunnel"],
      files_inbox: ["No", "tunnel"],
      env_secrets: ["No", "tunnel"],
      env_where: ["n/a"],
      config_sync: ["No", "tunnel"],
      preview_url: ["Yes, quick tunnels on trycloudflare.com", "quick"],
      preview_inspect: ["No", "tunnel"],
      tunnel_permanent: ["Yes, named tunnels", "private"],
      tunnel_domains: ["Yes, on a domain using Cloudflare DNS", "routing"],
      tunnel_protocols: ["HTTP, HTTPS, TCP, SSH, RDP and more", "routing"],
      private_network: ["With the WARP client", "private"],
      private_names: ["n/a"],
      setup_one_line: ["n/a"],
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
          "A named Cloudflare Tunnel serves a hostname on a domain whose DNS is on Cloudflare, so using your own domain means moving it there.",
          "Paperboat asks for two CNAME records at whichever DNS provider you already use and issues the certificate itself.",
        ],
        them: "$ cloudflared tunnel route dns api \\\n    api.example.com",
        us: "$ pb tunnel route add api --name web \\\n    --to http://localhost:3000\n$ pb tunnel domain add api api.example.com \\\n    --route web",
      },
      {
        title: "Quick links",
        body: [
          "Quick tunnels give a random trycloudflare.com address with no account, meant for testing.",
          "pb preview gives a named link under Paperboat's preview domain that you can make public, private to you, or open to your team, with a time limit you choose.",
        ],
        them: "$ cloudflared tunnel --url \\\n    http://localhost:3000",
        us: "$ pb preview 3000 --ttl 2h",
      },
      {
        title: "Working on the machine",
        body: [
          "Cloudflare Tunnel connects services; reaching a shell takes Cloudflare Access and cloudflared on the client.",
          ours.terminals,
        ],
      },
      {
        title: "Your own tunnel server",
        body: [
          "Traffic through a Cloudflare Tunnel flows through Cloudflare's network, on a domain whose DNS is at Cloudflare.",
          "Paperboat's tunnels work on any domain, and you can run the tunnel server yourself, so public traffic reaches a box you control.",
        ],
      },
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
    ],
  },
  {
    slug: "teleport",
    name: "Teleport",
    label: "Teleport",
    tagline: "Keyless SSH and recorded sessions, with no cluster to run.",
    headline: "Secure access, nothing to run",
    summary:
      "Teleport is an access platform a company runs for SSH, Kubernetes, databases and apps. Paperboat is one install per machine and a sign-in: keyless SSH, terminals that keep running, recordings and an audit log, ready in minutes.",
    gains: [
      "Keyless SSH with no cluster to operate",
      "Session recordings on every plan",
      "Terminals that keep running when you disconnect",
      "Previews, tunnels, files and secrets built in",
    ],
    cells: cells(teleportSources, {
      terminal_persistent: ["No", "matrix"],
      terminal_replay: ["Recorded sessions play back afterwards", "tsh"],
      terminal_browser: ["Yes, in the web UI", "matrix"],
      terminal_share: ["Yes, join sessions; moderation in Enterprise", "tsh"],
      terminal_record: ["Yes, session recording", "matrix"],
      exec: ["Yes, tsh ssh host command", "tsh"],
      ssh_keys: ["Yes, short-lived certificates", "matrix"],
      ssh_tools: ["Yes, tsh and OpenSSH with tsh config", "tsh"],
      ssh_vscode: ["Yes, with tsh config", "tsh"],
      files_send: ["Through tsh scp", "tsh"],
      files_inbox: ["No", "matrix"],
      env_secrets: ["No", "matrix"],
      env_where: ["n/a"],
      config_sync: ["No", "matrix"],
      preview_url: ["No, internal apps only", "matrix"],
      preview_inspect: ["No", "matrix"],
      tunnel_permanent: ["No public tunnels", "matrix"],
      tunnel_domains: ["No public tunnels", "matrix"],
      tunnel_protocols: ["SSH, Kubernetes, databases, apps, desktops", "matrix"],
      private_network: ["No; access goes through its proxy", "matrix"],
      private_names: ["Per app, through its proxy", "matrix"],
      setup_one_line: ["Joins with a join token; the rest is yours", "join"],
      encryption_native: ["TLS to Teleport's proxy", "matrix"],
      encryption_public: ["n/a"],
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
          "Teleport runs a cluster (auth service and proxy) that every connection goes through; it is built for many servers, many people and compliance.",
          ours.install,
        ],
      },
      {
        title: "Sessions",
        body: [
          "Teleport records sessions and plays them back afterwards; people can join a live session, and moderated sessions are an Enterprise feature. A session ends when its connection does.",
          ours.terminals,
        ],
        them: "$ tsh ssh dev@devbox\n$ tsh join <session-id>",
        us: "$ pb devbox\n$ pb session share S12 --team acme --all",
      },
      {
        title: "Public links",
        body: [
          "Teleport's application access puts internal apps behind its proxy for signed-in users; it doesn't publish a port to the internet.",
          ours.previews,
        ],
        us: "$ pb preview 3000",
      },
      {
        title: "Audit and compliance",
        body: [
          "Teleport records sessions and keeps an audit log, on infrastructure your company runs.",
          "Paperboat has roles, single sign-on, directory sync and an audit log you can export or stream. Terminals can be recorded on every plan, encrypted on the machine; on Team, by rule too, with an archive in your own S3 bucket. Every replay is in the audit log.",
        ],
      },
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
      "Paperboat runs beside Teleport while you move machines over.",
    ],
    faq: [
      {
        q: "Does Paperboat record sessions?",
        a: "Yes, on every plan. Paperboat terminals can be recorded and played back in the console or the terminal, kept encrypted on the machine. Team adds recording by rule and an archive in your own S3 bucket. Every replay is in the audit log.",
      },
      {
        q: "Can I self-host Paperboat like Teleport?",
        a: "You can run the relays and tunnel servers that carry your traffic yourself, on every plan. Accounts and sign-in are hosted, so there is nothing else to operate.",
      },
    ],
  },
  {
    slug: "netbird",
    name: "NetBird",
    label: "NetBird",
    tagline: "Private access to your machines, and everything you do once you are there.",
    headline: "A private network, and the work on top",
    summary:
      "NetBird is an open-source WireGuard network between your devices. Paperboat connects your machines privately too, and adds what you do once you're connected: terminals, files, secrets, previews and tunnels.",
    gains: [
      "Terminals that keep running when you disconnect",
      "An HTTPS address for every port, by name",
      "Public previews and tunnels",
      "Nothing to host, or host your own relays",
    ],
    cells: cells(netbirdSources, {
      terminal_persistent: ["No", "ssh"],
      terminal_replay: ["No", "ssh"],
      terminal_browser: ["Yes, SSH from the dashboard", "ssh"],
      terminal_share: ["No", "ssh"],
      terminal_record: ["No", "ssh"],
      exec: ["Through ssh", "ssh"],
      ssh_keys: ["Signs in with your identity provider", "ssh"],
      ssh_tools: ["ssh, scp and sftp", "ssh"],
      ssh_vscode: ["As plain SSH over the network", "ssh"],
      files_send: ["No", "docs"],
      files_inbox: ["No", "docs"],
      env_secrets: ["No", "docs"],
      env_where: ["n/a"],
      config_sync: ["No", "docs"],
      preview_url: ["No", "docs"],
      preview_inspect: ["No", "docs"],
      tunnel_permanent: ["No", "docs"],
      tunnel_domains: ["No", "docs"],
      tunnel_protocols: ["n/a"],
      private_network: ["Yes, a WireGuard mesh", "docs"],
      private_names: ["A DNS name per peer", "docs"],
      setup_one_line: ["Joins with a setup key and groups; the rest is yours", "setupKeys"],
      encryption_native: ["End to end (WireGuard)", "docs"],
      encryption_public: ["n/a"],
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
          "NetBird builds a private WireGuard network with policies and leaves the work to your usual tools; its dashboard can open an SSH session in the browser.",
          ours.install,
        ],
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "NetBird SSH sessions end when the connection drops, like any SSH session.",
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
          "NetBird is open source and you can run the whole service on your own servers.",
          "With Paperboat there is no management server to run. You can still run your own relays and tunnel servers, on every plan, so relayed and public traffic stays on servers you choose.",
        ],
      },
      {
        title: "Recordings and new machines",
        body: [
          "NetBird documents no session recording, and new peers join with a setup key. Setting them up after that is up to you.",
          ours.extras,
        ],
        us: "$ pb recordings start\n$ pb recipe create -f ci.toml",
      },
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
      "The two run side by side while you move over.",
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
    tagline: "Every port by name, over HTTPS, with no network to plan.",
    headline: "Every machine, every port, no network to plan",
    summary:
      "ZeroTier makes your devices act as if they shared one Ethernet network. Paperboat gives you encrypted access to every machine and every port by name, with nothing to plan, and the tools to work there.",
    gains: [
      "Every port by name, with real HTTPS",
      "No address ranges or network rules to manage",
      "Terminals, SSH and files built in",
      "Public previews and tunnels",
    ],
    cells: cells(zerotierSources, {
      terminal_persistent: ["No", "protocol"],
      terminal_replay: ["No", "protocol"],
      terminal_browser: ["No", "protocol"],
      terminal_share: ["No", "protocol"],
      terminal_record: ["No", "protocol"],
      exec: ["No", "protocol"],
      ssh_keys: ["No", "protocol"],
      ssh_tools: ["Your own SSH over the network", "protocol"],
      ssh_vscode: ["Your own SSH over the network", "protocol"],
      files_send: ["No", "protocol"],
      files_inbox: ["No", "protocol"],
      env_secrets: ["No", "protocol"],
      env_where: ["n/a"],
      config_sync: ["No", "protocol"],
      preview_url: ["No", "protocol"],
      preview_inspect: ["No", "protocol"],
      tunnel_permanent: ["No", "protocol"],
      tunnel_domains: ["No", "protocol"],
      tunnel_protocols: ["n/a"],
      private_network: ["Yes, a virtual Ethernet network", "protocol"],
      private_names: ["n/a"],
      setup_one_line: ["Joins a network by ID, then waits for approval", "controller"],
      encryption_native: ["End to end; its relays can't read traffic", "protocol"],
      encryption_public: ["n/a"],
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
          "ZeroTier emulates Ethernet, so broadcast, multicast and bridging work as on a local network; everything else is up to your own tools.",
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
        title: "The machines you work on",
        body: [
          "ZeroTier runs on routers, NAS boxes and other devices, and can bridge a whole physical network.",
          "Paperboat runs on the Linux, macOS and Windows machines you work on, and gives each of their ports a private HTTPS name.",
        ],
      },
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
      "The two run side by side while you move over.",
    ],
    faq: [
      {
        q: "How do I reach a machine's ports?",
        a: "By name: each web port has its own private HTTPS address, and anything else that speaks TCP uses the machine's own local address.",
      },
    ],
  },
  {
    slug: "twingate",
    name: "Twingate",
    label: "Twingate",
    tagline: "Private access with terminals, files and secrets built in.",
    headline: "Access to machines, and the tools to use them",
    summary:
      "Twingate gives a company's people access to private resources through connectors and identity policies. Paperboat connects your machines to each other directly and gives you the tools to work on them.",
    gains: [
      "No connectors to deploy",
      "Terminals, SSH and files on every machine",
      "Encrypted secrets and config sync",
      "Public previews and tunnels",
    ],
    cells: cells(twingateSources, {
      terminal_persistent: ["No", "ssh"],
      terminal_replay: ["No", "ssh"],
      terminal_browser: ["No", "ssh"],
      terminal_share: ["No", "ssh"],
      terminal_record: ["Through an SSH gateway you deploy", "recording"],
      exec: ["Through SSH", "ssh"],
      ssh_keys: ["Yes, short-lived certificates (Privileged Access)", "ssh"],
      ssh_tools: ["Yes, ssh, sftp and rsync", "ssh"],
      ssh_vscode: ["As SSH to a resource", "ssh"],
      files_send: ["Through sftp or rsync", "ssh"],
      files_inbox: ["No", "ssh"],
      env_secrets: ["No", "faq"],
      env_where: ["n/a"],
      config_sync: ["No", "faq"],
      preview_url: ["No", "faq"],
      preview_inspect: ["No", "faq"],
      tunnel_permanent: ["No, private resources only", "resources"],
      tunnel_domains: ["Private names only", "resources"],
      tunnel_protocols: ["TCP, UDP and ICMP, privately", "resources"],
      private_network: ["People reach resources, not machines each other", "resources"],
      private_names: ["Resources keep their own names", "resources"],
      setup_one_line: ["n/a"],
      encryption_native: ["End to end, client to connector", "relays"],
      encryption_public: ["n/a"],
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
          "Twingate connects users' devices to resources in private networks through connectors; it is built for giving employees access.",
          "Paperboat connects the machines you install it on to each other, and lets you work on them from anywhere.",
        ],
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "Twingate's Privileged Access for SSH signs SSH connections with short-lived certificates; the session ends with the connection.",
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
      {
        title: "Recordings and new machines",
        body: [
          "Twingate records SSH sessions through a gateway you deploy in your own network.",
          ours.extras,
        ],
        us: "$ pb recordings start\n$ pb recipe create -f ci.toml",
      },
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
      "Give your team access to machines with a few rules, and send them the SSH host names.",
    ],
    faq: [
      {
        q: "Who can reach a machine?",
        a: "Only people a rule allows, on that machine and those ports, from their own signed-in machines. Every action is in the audit log.",
      },
    ],
  },
  {
    slug: "cloudflare-zero-trust",
    name: "Cloudflare Zero Trust",
    label: "Cloudflare Zero Trust",
    tagline: "Private access to your machines with no gateway or connector to run.",
    headline: "Your machines, without the perimeter",
    summary:
      "Cloudflare Zero Trust puts a company's apps and networks behind Cloudflare, with identity policies and filtering. Paperboat connects your own machines directly, encrypted end to end, with no gateway, client profile or DNS change.",
    gains: [
      "Encrypted end to end between your machines",
      "No gateway or connector to run",
      "Terminals, SSH, files and secrets built in",
      "Tunnels on your own domain, wherever its DNS is",
    ],
    cells: cells(cfZeroTrustSources, {
      terminal_persistent: ["No", "setup"],
      terminal_replay: ["No", "setup"],
      terminal_browser: ["Yes, browser SSH and RDP", "setup"],
      terminal_share: ["No", "setup"],
      terminal_record: ["SSH command logs, through Access for Infrastructure", "sshLogs"],
      exec: ["Through SSH", "ssh"],
      ssh_keys: ["Yes, short-lived certificates", "ssh"],
      ssh_tools: ["Yes, through WARP or cloudflared", "ssh"],
      ssh_vscode: ["As SSH through WARP", "ssh"],
      files_send: ["No", "setup"],
      files_inbox: ["No", "setup"],
      env_secrets: ["No", "setup"],
      env_where: ["n/a"],
      config_sync: ["No", "setup"],
      preview_url: ["Through Tunnel, set up in the dashboard", "tunnel"],
      preview_inspect: ["No", "tunnel"],
      tunnel_permanent: ["Yes, named tunnels", "tunnel"],
      tunnel_domains: ["Yes, on a domain using Cloudflare DNS", "routing"],
      tunnel_protocols: ["HTTP, HTTPS, TCP, SSH, RDP and more", "routing"],
      private_network: ["Yes, routed through Cloudflare", "private"],
      private_names: ["Private hostnames through the WARP client", "private"],
      setup_one_line: ["Devices enroll in the WARP client; the rest is yours", "warp"],
      encryption_native: ["Through Cloudflare, which can inspect it", "private"],
      encryption_public: ["Ends at Cloudflare", "tunnel"],
      teams: ["Yes, seats in your organization", "users"],
      access_rules: ["Yes, identity and device policies", "policies"],
      api_tokens: ["Yes, Cloudflare API tokens", "api"],
      selfhost: ["The connector only", "tunnel"],
      platforms: ["Linux, macOS, Windows, iOS, Android, ChromeOS", "warp"],
      pricing_model: ["Per user: $7 a month", "pricing"],
      free_tier: ["Up to 50 users", "pricing"],
    }),
    differences: [
      {
        title: "A company's perimeter, or your machines",
        body: [
          "Cloudflare Zero Trust secures a whole company: who may reach which app, from which device, with web filtering and logs.",
          ours.install,
        ],
      },
      {
        title: "Who can read the traffic",
        body: [
          "With WARP, your traffic goes through Cloudflare, which applies policies and can inspect it.",
          "Paperboat encrypts between your own devices end to end and connects them directly when it can; relays forward traffic they can't read. Only public links end TLS at Paperboat's edge.",
        ],
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "Access can render SSH in the browser; the session ends when the tab or connection does.",
          ours.terminals,
        ],
        us: "$ pb devbox",
      },
      {
        title: "Your DNS stays where it is",
        body: [
          "Public hostnames on Cloudflare need the domain's DNS on Cloudflare.",
          "Paperboat asks for two CNAME records at whichever DNS provider you already use.",
        ],
        us: "$ pb tunnel route add api --name web \\\n    --to http://localhost:3000\n$ pb tunnel domain add api api.example.com \\\n    --route web",
      },
      {
        title: "Recordings and new machines",
        body: [
          "Cloudflare logs SSH commands through Access for Infrastructure, encrypted with a key you provide.",
          ours.extras,
        ],
        us: "$ pb recordings start\n$ pb recipe create -f ci.toml",
      },
    ],
    pricing: {
      lines: ["Free: up to 50 users", "Pay-as-you-go: $7 per user a month", "Contract plans: by quote"],
      source: cfZeroTrustSources.pricing,
    },
    switching: [
      install,
      "Move your machines over one at a time; the two run side by side.",
    ],
    faq: [
      {
        q: "Is the traffic encrypted end to end?",
        a: "Yes, between your machines. Relays pass on only encrypted bytes. Public links end HTTPS at the tunnel server, which you can also run yourself.",
      },
    ],
  },
  {
    slug: "enterprise-vpn",
    name: "an enterprise VPN",
    label: "Enterprise VPN",
    tagline: "Your team reaches the machines it needs, with no VPN client.",
    headline: "Reach your machines, skip the VPN",
    summary:
      "Enterprise VPNs such as Zscaler, Prisma Access, Cisco Secure Client and OpenVPN connect employees to a company network. Paperboat connects developers to the machines they work on: one install, a sign-in, and every machine is there.",
    gains: [
      "One install per machine and a sign-in",
      "No gateway to run or patch",
      "Access per machine and per port",
      "Terminals, files, secrets and previews built in",
    ],
    cells: cells(vpnSources, {
      terminal_persistent: ["No", "zpa"],
      terminal_replay: ["No", "zpa"],
      terminal_browser: ["Some, as browser access to apps", "zpa"],
      terminal_share: ["No", "zpa"],
      terminal_record: ["n/a"],
      exec: ["No; SSH once connected", "openvpn"],
      ssh_keys: ["No", "openvpn"],
      ssh_tools: ["Your own SSH once connected", "openvpn"],
      ssh_vscode: ["Your own SSH once connected", "openvpn"],
      files_send: ["No", "openvpn"],
      files_inbox: ["No", "openvpn"],
      env_secrets: ["No", "openvpn"],
      env_where: ["n/a"],
      config_sync: ["No", "openvpn"],
      preview_url: ["No", "openvpn"],
      preview_inspect: ["No", "openvpn"],
      tunnel_permanent: ["No", "openvpn"],
      tunnel_domains: ["No", "openvpn"],
      tunnel_protocols: ["n/a"],
      private_network: ["Into the company network", "prisma"],
      private_names: ["n/a"],
      setup_one_line: ["n/a"],
      encryption_native: ["To the gateway or the vendor's cloud", "zpa"],
      encryption_public: ["n/a"],
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
          "Paperboat is for the machines you work on: your laptop, a server, a box under a desk. Each one is reachable by name, port by port, with nothing to route.",
        ],
      },
      {
        title: "Nothing to run",
        body: [
          "These products need gateways, connectors or a cloud tenant set up and maintained by someone.",
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
    pricing: {
      lines: [
        "Zscaler, Prisma Access, Cisco: by quote",
        "OpenVPN Access Server: free for 2 connections, then $7 per connection a month",
      ],
      source: vpnSources.openvpnPricing,
    },
    switching: [
      install,
      "Move development machines over first. Paperboat works while the VPN is connected.",
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
    tagline: "SSH to every machine with no keys to manage and no bastion to patch.",
    headline: "SSH, without the bastion",
    summary:
      "Plain OpenSSH through a bastion host is the classic way in, with tmux to keep work running. Paperboat keeps your SSH tools and takes away the rest: no open port, no keys to hand out, and terminals that keep running on their own.",
    gains: [
      "No open port and no bastion to patch",
      "Keys handled for you",
      "Terminals that keep running, no tmux needed",
      "Files, secrets and previews built in",
    ],
    cells: cells(sshSources, {
      terminal_persistent: ["With tmux or screen", "tmux"],
      terminal_replay: ["With tmux scrollback", "tmux"],
      terminal_browser: ["No", "ssh"],
      terminal_share: ["With a shared tmux session", "tmux"],
      terminal_record: ["No", "sshd"],
      exec: ["Yes, ssh host command", "ssh"],
      ssh_keys: ["You manage keys on every host", "sshd"],
      ssh_tools: ["Yes, the original", "scp"],
      ssh_vscode: ["Yes, with ProxyJump", "config"],
      files_send: ["scp or rsync", "scp"],
      files_inbox: ["No", "scp"],
      env_secrets: ["No; SetEnv sends plain variables", "config"],
      env_where: ["n/a"],
      config_sync: ["No", "openssh"],
      preview_url: ["Only with ssh -R to a server you run", "ssh"],
      preview_inspect: ["No", "ssh"],
      tunnel_permanent: ["On servers you run", "ssh"],
      tunnel_domains: ["On servers you run", "ssh"],
      tunnel_protocols: ["TCP forwarding", "ssh"],
      private_network: ["Port forwarding through the bastion", "ssh"],
      private_names: ["Port forwarding by hand", "ssh"],
      setup_one_line: ["Your own scripts", "openssh"],
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
          "A bastion needs a public address, an open port 22 and someone to keep it patched and its keys in order.",
          "Paperboat machines make only outbound connections and reach each other directly or through relays; there is nothing to expose.",
        ],
        them: "$ ssh -J bastion.example.com dev@10.0.1.5",
        us: "$ pb ssh devbox",
      },
      {
        title: "Persistence built in",
        body: [
          "With SSH you add tmux or screen on every host to keep work running and to share it.",
          ours.terminals,
        ],
        them: "$ ssh devbox -t tmux new -A -s work",
        us: "$ pb devbox",
      },
      {
        title: "Your tools keep working",
        body: [
          "Paperboat generates SSH host names, so plain ssh, scp, rsync, VS Code Remote-SSH and anything else built on OpenSSH work as before, without keys to copy around.",
        ],
        us: "$ ssh devbox.{localDomain}\n$ rsync -a ./site devbox.{localDomain}:site",
      },
      {
        title: "Public links",
        body: ["Reverse forwarding (ssh -R) needs a public server you run.", ours.previews],
        them: "$ ssh -R 80:localhost:3000 \\\n    server.example.com",
        us: "$ pb preview 3000",
      },
      {
        title: "Recordings and new machines",
        body: [
          "OpenSSH doesn't record sessions, and setting up a new server is a job for your own scripts.",
          ours.extras,
        ],
        us: "$ pb recordings start\n$ pb recipe create -f ci.toml",
      },
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
    tagline: "Remote work from any editor, terminal or browser, not just VS Code.",
    headline: "Remote work in any tool",
    summary:
      "VS Code Remote Tunnels opens a remote machine in VS Code or vscode.dev. Paperboat works with any editor, terminal, browser or AI agent, and adds terminals that keep running, files, secrets and permanent tunnels.",
    gains: [
      "Any editor, terminal or AI agent",
      "Terminals that keep running when you disconnect",
      "Permanent tunnels on your own domain",
      "Plain SSH for every tool",
    ],
    cells: cells(vscodeSources, {
      terminal_persistent: ["No; only while VS Code runs there", "tunnels"],
      terminal_replay: ["No", "tunnels"],
      terminal_browser: ["Yes, in vscode.dev", "tunnels"],
      terminal_share: ["No, one user at a time", "tunnels"],
      terminal_record: ["No", "tunnels"],
      exec: ["No", "tunnels"],
      ssh_keys: ["Signs in with GitHub or Microsoft", "tunnels"],
      ssh_tools: ["No; Remote-SSH is separate", "overview"],
      ssh_vscode: ["It is VS Code's own alternative", "overview"],
      files_send: ["No", "tunnels"],
      files_inbox: ["No", "tunnels"],
      env_secrets: ["No", "tunnels"],
      env_where: ["n/a"],
      config_sync: ["VS Code settings sync only", "overview"],
      preview_url: ["Yes, a forwarded port made public", "ports"],
      preview_inspect: ["In browser DevTools; no replay", "devtunnels"],
      tunnel_permanent: ["Yes, persistent dev tunnels", "cli"],
      tunnel_domains: ["No", "cli"],
      tunnel_protocols: ["HTTP, HTTPS, WebSocket", "cli"],
      private_network: ["No", "tunnels"],
      private_names: ["Forwarded ports, one at a time", "ports"],
      setup_one_line: ["n/a"],
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
          "Remote Tunnels is built for VS Code: the machine opens in VS Code or vscode.dev, signed in with the same GitHub or Microsoft account on both ends.",
          "Paperboat works with any terminal, editor or agent, including VS Code through plain Remote-SSH.",
        ],
        them: "$ code tunnel",
        us: "$ pb devbox\n$ ssh devbox.{localDomain}",
      },
      {
        title: "Terminals that survive a dropped connection",
        body: [
          "The machine is reachable while VS Code runs there, and one person uses it at a time.",
          ours.terminals,
        ],
      },
      {
        title: "Public links and domains",
        body: [
          "A forwarded port can be made public, and dev tunnels can keep a stable address on Microsoft's domain; your own domains aren't supported.",
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
