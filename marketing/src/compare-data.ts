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
  /** Short answers for the overview matrix on /compare, in rowNames order. */
  matrix: string[]
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
  "Share a terminal or a preview link on any plan; Team adds members with lasting access to each other’s machines",
  "Run your own relays and tunnel servers",
  "Free to start with everything for one person; plans differ in bandwidth; Team is per seat",
]

/** Paperboat's column in the overview matrix. */
export const usMatrix = [
  "Yes, survives disconnects",
  "Yes, keys managed",
  "Yes",
  "Yes",
  "Yes",
  "Yes, between machines",
  "Yes",
  "Relays and tunnel servers",
  "Free; Pro $8/mo; Team $15/seat",
]

/** The rows every comparison answers, in this order. */
export const rowNames = [
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
  matrix: string[],
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
    matrix,
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
  // Sources: https://tailscale.com/kb/1193/tailscale-ssh, https://tailscale.com/kb/1223/funnel
  // (ts.net names only), https://tailscale.com/kb/1018/acls, https://tailscale.com/pricing,
  // https://github.com/juanfont/headscale (community project).
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
    ["SSH only", "Over the network", "Funnel (ts.net)", "No custom domains", "Yes, mesh VPN", "Yes", "Yes, ACLs", "Headscale (community)", "Free personal; per user"],
    ["You want a full VPN: every device and subnet on one private network.", "You need detailed network access rules for a whole company."],
    ["You mostly want to work on your machines: terminals, SSH, previews and files.", "You want public links and permanent tunnels in the same tool."],
    switchSteps("Tailscale"),
  ),
  // Sources: https://ngrok.com/docs/universal-gateway/custom-domains/, https://ngrok.com/pricing,
  // https://ngrok.com/docs/traffic-policy/ (TLS terminates at ngrok's edge).
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
    ["No", "No", "Yes", "Paid plans", "No", "Ends at ngrok", "Paid plans", "No", "Free tier; paid plans"],
    ["You only need public URLs, with traffic features like request inspection and policies at scale.", "You need its integrations and API gateway features."],
    ["You also want to reach and work on the machine itself, not just share a port.", "You want previews and permanent tunnels with one install."],
    switchSteps("ngrok"),
  ),
  // Sources: https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/
  // (incl. Quick Tunnels on trycloudflare.com), .../applications/non-http/ (SSH),
  // https://www.cloudflare.com/plans/zero-trust-services/.
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
    ["Browser SSH via Access", "Via cloudflared", "Quick tunnels", "Cloudflare DNS only", "With WARP", "Ends at Cloudflare", "Zero Trust policies", "No", "Tunnels free"],
    ["Your domain is already on Cloudflare and you want its security and caching in front.", "You need Cloudflare’s Zero Trust policies across a company."],
    ["You don’t want to move your DNS to Cloudflare.", "You want terminals, previews and files on the same machines, not only tunnels."],
    switchSteps("cloudflared"),
  ),
  // Sources: https://goteleport.com/docs/ (server access, session recording, app access),
  // https://goteleport.com/pricing/ (Community Edition, Enterprise).
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
    ["Yes, recorded", "Yes, certificates", "Internal apps only", "No", "No", "To its proxy", "Yes, detailed", "Yes", "Free Community; paid Enterprise"],
    ["You need audit, compliance and access requests across many servers.", "You manage Kubernetes clusters and databases for a company."],
    ["You want fast access to your own machines without running an access platform.", "You want public previews and tunnels too."],
    switchSteps("Teleport"),
  ),
  // Sources: https://docs.netbird.io/how-to/ssh (NetBird SSH), https://docs.netbird.io/how-to/manage-network-access
  // (policies), https://docs.netbird.io/selfhosted/selfhosted-guide, https://netbird.io/pricing.
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
    ["SSH only", "Over the network", "No", "No", "Yes, mesh VPN", "Yes", "Yes, policies", "Yes, fully", "Free tier; per user"],
    ["You want a fully open-source VPN you can host end to end.", "You need network access rules across a company."],
    ["You want terminals, previews, public links and file sending in one tool.", "You’d rather not run a network control server yourself."],
    switchSteps("NetBird"),
  ),
  // Sources: https://docs.zerotier.com/protocol (Salsa20/Poly1305 end to end, emulated Ethernet,
  // roots relay when direct fails), https://docs.zerotier.com/controller (own controller),
  // https://www.zerotier.com/pricing/ (Personal free, 10 devices; Essential $18/month).
  entry(
    "zerotier",
    "ZeroTier",
    "ZeroTier makes your devices act like they share one Ethernet network. Paperboat is built around working on your machines: terminals, previews, tunnels and files.",
    [
      "None; use your own SSH over the network",
      "Regular SSH tools over the virtual network",
      "None",
      "None",
      "Yes, a virtual Ethernet network between devices",
      "Yes, between devices; its relays can’t read traffic",
      "Network members and flow rules",
      "You can run your own network controller",
      "Free for 10 devices; paid plans per device",
    ],
    ["No", "Over the network", "No", "No", "Yes, virtual Ethernet", "Yes", "Network rules", "Own controller", "Free 10 devices; per device"],
    ["You need a virtual LAN: broadcast, multicast or bridging, not just IP between devices.", "You want to connect devices that run ZeroTier already, such as routers and NAS boxes."],
    ["You want terminals, previews, public links and file sending, not only a network.", "You’d rather not manage network rules to reach your own machines."],
    switchSteps("ZeroTier"),
  ),
  // Sources: https://www.twingate.com/docs/how-twingate-works (client to connector, controller is
  // Twingate's service), https://www.twingate.com/pricing (Starter free up to 5 users; Teams $5,
  // Business $10 per user/month yearly; SSH privileged access).
  entry(
    "twingate",
    "Twingate",
    "Twingate gives a company’s people access to private resources without a VPN. Paperboat is for working on machines directly: terminals, SSH, previews, tunnels and files.",
    [
      "None; SSH sessions through its privileged access on higher plans",
      "SSH to resources it protects",
      "None",
      "None",
      "Access from clients to resources, not between machines",
      "Encrypted from the client to your connector",
      "Groups, policies and device checks",
      "Connectors only; the controller is Twingate’s service",
      "Free up to 5 users; paid plans per user",
    ],
    ["No", "Yes, to resources", "No", "No", "Client to resource", "To your connector", "Yes, policies", "Connectors only", "Free 5 users; per user"],
    ["You run a company network and need people to reach internal resources with device checks.", "You want access policies tied to your identity provider for many users."],
    ["You want to work on your own machines from anywhere, with terminals and files.", "You want public previews and permanent tunnels, which Twingate doesn’t offer."],
    switchSteps("Twingate"),
  ),
  // Sources: https://developers.cloudflare.com/cloudflare-one/ (Access, WARP, Tunnel),
  // https://developers.cloudflare.com/cloudflare-one/applications/non-http/browser-rendering/
  // (browser SSH), https://www.cloudflare.com/plans/zero-trust-services/ (free up to 50 users,
  // pay-as-you-go per user).
  entry(
    "cloudflare-zero-trust",
    "Cloudflare Zero Trust",
    "Cloudflare Zero Trust (Access and the WARP client) puts a company’s apps and networks behind Cloudflare. Paperboat is a tool for working on your own machines, with no DNS move.",
    [
      "Browser SSH through Access",
      "SSH through cloudflared or WARP, with Access policies",
      "Through Cloudflare Tunnel on your Cloudflare domain",
      "Yes, on a domain that uses Cloudflare DNS",
      "Yes, WARP clients reach private networks through Cloudflare",
      "Traffic is decrypted at Cloudflare for inspection",
      "Identity policies, device posture and logs",
      "No",
      "Free up to 50 users; paid per user",
    ],
    ["Browser SSH", "Yes, with policies", "Via Tunnel", "Cloudflare DNS only", "Yes, via WARP", "Ends at Cloudflare", "Yes, policies", "No", "Free 50 users; per user"],
    ["You secure a whole company: web filtering, device posture and app access in one place.", "Your domains are already on Cloudflare."],
    ["You want terminals that survive disconnects, previews and file sending on your own machines.", "You want your machines to talk to each other directly, encrypted end to end."],
    switchSteps("Cloudflare Zero Trust"),
  ),
  // Sources: https://www.zscaler.com/products-and-solutions/zscaler-private-access,
  // https://www.paloaltonetworks.com/sase/access, https://www.cisco.com/site/us/en/products/security/secure-client/,
  // https://openvpn.net/access-server/pricing/ (self-hosted; free for 2 connections, then per connection).
  entry(
    "enterprise-vpn",
    "an enterprise VPN",
    "Zscaler Private Access, Prisma Access, Cisco Secure Client (AnyConnect) and OpenVPN Access Server connect employees to a company network. Paperboat connects you to your own machines and lets you work on them.",
    [
      "None; use SSH once connected",
      "Regular SSH tools once connected",
      "None",
      "None",
      "Access into the company network, not between your machines",
      "Encrypted to the company’s gateway or the vendor’s cloud",
      "Detailed policies tied to your identity provider",
      "OpenVPN Access Server and Cisco gateways run on your servers; Zscaler and Prisma are cloud services",
      "Sales quotes; OpenVPN Access Server is free for 2 connections",
    ],
    ["No", "Once connected", "No", "No", "Into a network", "To a gateway", "Yes, detailed", "Some", "Quotes; per user"],
    ["Your company needs every employee on the corporate network with central policy and compliance.", "Your security team already runs one of these."],
    ["You’re a developer or small team who wants your own machines, not a corporate network.", "You want to be working in a minute, without a gateway to run."],
    switchSteps("the VPN"),
  ),
  // Sources: https://man.openbsd.org/ssh (ProxyJump -J, -L/-R forwarding; end-to-end encryption
  // through a jump host), https://man.openbsd.org/sshd_config.
  entry(
    "ssh-bastion",
    "SSH and a bastion",
    "Plain SSH with a bastion (jump) host is the classic way in. Paperboat keeps SSH, adds terminals that survive disconnects, previews and tunnels, and needs no open port.",
    [
      "Yes; add tmux or screen to keep work running",
      "Yes, the original",
      "Only with ssh -R to a server you run",
      "Only on servers you run",
      "Port forwarding through the bastion, not a network",
      "Yes, end to end through a jump host",
      "Unix users and keys on every host",
      "Everything is yours to run and patch",
      "Free software; you pay for the bastion and your time",
    ],
    ["With tmux", "Yes", "With your own server", "With your own server", "Forwarding only", "Yes", "Users and keys", "All of it", "Free; you run it"],
    ["You already run servers with public addresses and keys you manage well.", "You want nothing but OpenSSH."],
    ["Your machines sit behind NAT or at home, with no open port.", "You want keys, sharing and previews handled for you, and terminals that come back after a dropped connection."],
    switchSteps("your bastion"),
  ),
  // Sources: https://code.visualstudio.com/docs/remote/tunnels (Microsoft dev tunnels service; SSH
  // inside the tunnel for end-to-end encryption; same GitHub or Microsoft account on both ends;
  // 10 tunnels per account, bandwidth limits), https://code.visualstudio.com/docs/debugtest/port-forwarding
  // (public port visibility through dev tunnels).
  entry(
    "vscode-tunnels",
    "VS Code Remote Tunnels",
    "VS Code Remote Tunnels opens a remote machine in VS Code or vscode.dev. Paperboat works with any terminal, editor or browser, and adds previews, tunnels and sharing.",
    [
      "Inside VS Code or vscode.dev only",
      "None; Remote-SSH is a separate extension",
      "Yes, a forwarded port can be made public",
      "None",
      "None",
      "Yes, SSH inside the tunnel through Microsoft’s relay",
      "Only your own account; one user at a time",
      "No; it runs on Microsoft dev tunnels",
      "Free, with tunnel and bandwidth limits",
    ],
    ["In VS Code", "No", "Yes, public ports", "No", "No", "Yes", "Just you", "No", "Free, with limits"],
    ["You only want to open a remote folder in VS Code.", "You already sign in to VS Code with GitHub and need nothing else."],
    ["You use other editors, plain terminals, agents or a phone.", "You want permanent tunnels, your own domains, sharing with a team, or plain SSH."],
    switchSteps("VS Code tunnels"),
  ),
]
