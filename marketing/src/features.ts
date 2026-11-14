/**
 * The feature pages, in one place: the header, the footer, the features index and the home page all
 * read this list. `command` is the one line that best shows the feature; every command here is in
 * the CLI reference (cli/docs/cli).
 */
export type Feature = {
  slug: string
  name: string
  /** What you can do, in one plain sentence. */
  line: string
  command: string
  /** Phosphor icon name, shown beside the feature in lists. */
  icon: string
  group: "Reach your machines" | "Put things online" | "Keep it together"
}

export const features: Feature[] = [
  {
    slug: "terminal",
    icon: "terminal-window",
    name: "Terminal",
    line: "A shell on the machine that keeps running when you disconnect, and shows what you missed.",
    command: "pb connect studio",
    group: "Reach your machines",
  },
  {
    slug: "exec",
    icon: "lightning",
    name: "Remote commands",
    line: "Run one command on another machine and get its output and exit code.",
    command: "pb exec studio -- git status",
    group: "Reach your machines",
  },
  {
    slug: "ssh",
    icon: "key",
    name: "SSH",
    line: "ssh, scp, sftp and rsync with no keys to hand out and no port to open.",
    command: "pb ssh studio",
    group: "Reach your machines",
  },
  {
    slug: "files",
    icon: "paper-plane-tilt",
    name: "Files",
    line: "Send files straight into another machine's Inbox. Interrupted sends resume.",
    command: "pb send ./build.tar.gz --to studio",
    group: "Reach your machines",
  },
  {
    slug: "private-access",
    icon: "lock-key",
    name: "Private access",
    line: "Open a port on another machine as if it were local, without publishing it.",
    command: "pb access machine studio --port 5432",
    group: "Reach your machines",
  },
  {
    slug: "previews",
    icon: "globe-simple",
    name: "Previews",
    line: "A public HTTPS link to a local port, with an inspector to see and replay each request.",
    command: "pb preview 3000",
    group: "Put things online",
  },
  {
    slug: "tunnels",
    icon: "plugs-connected",
    name: "Tunnels and domains",
    line: "A permanent public address for a service, on your own domain if you want.",
    command: "pb tunnel create api --port 8080",
    group: "Put things online",
  },
  {
    slug: "self-hosting",
    icon: "hard-drives",
    name: "Self-hosting",
    line: "Run the relay and the tunnel server on your own Linux box.",
    command: "pbh status",
    group: "Put things online",
  },
  {
    slug: "env",
    icon: "vault",
    name: "ENV",
    line: "Environment variables encrypted on your device and delivered to your machines.",
    command: "pb env run -- npm test",
    group: "Keep it together",
  },
  {
    slug: "config-sync",
    icon: "arrows-clockwise",
    name: "Config sync",
    line: "The same dotfiles on every machine, from a Git repository you own.",
    command: "pb config sync apply",
    group: "Keep it together",
  },
  {
    slug: "teams",
    icon: "users-three",
    name: "Teams and access",
    line: "Share a machine with people, one capability at a time, with a policy file and an audit log.",
    command: "pb policy explain",
    group: "Keep it together",
  },
  {
    slug: "console",
    icon: "browser",
    name: "Web console",
    line: "Your machines in a browser: terminals, files, previews, access rules and billing.",
    command: "pb machine add",
    group: "Keep it together",
  },
]

export const featureGroups = ["Reach your machines", "Put things online", "Keep it together"] as const
export const feature = (slug: string) => {
  const found = features.find((item) => item.slug === slug)
  if (!found) throw new Error(`features: unknown feature ${slug}`)
  return found
}

export const useCases = [
  {
    slug: "ai-agents",
    name: "AI agents",
    line: "Let a coding agent run commands on your other machines, with a token that can do only that.",
  },
  {
    slug: "homelab",
    name: "Home lab",
    line: "Reach the server at home from anywhere without forwarding a port on your router.",
  },
  {
    slug: "small-teams",
    name: "Small teams",
    line: "A shared staging box, review links and team secrets, without running a VPN.",
  },
] as const
