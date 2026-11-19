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
  group: "Work on your machines" | "Put things online" | "Set up and share"
}

export const features: Feature[] = [
  {
    slug: "terminal",
    icon: "terminal-window",
    name: "Terminals",
    line: "A shell on any machine that keeps running when you close the laptop.",
    command: "pb connect studio",
    group: "Work on your machines",
  },
  {
    slug: "exec",
    icon: "lightning",
    name: "Remote commands",
    line: "Run a command on another machine and get the result back.",
    command: "pb exec studio -- git status",
    group: "Work on your machines",
  },
  {
    slug: "ssh",
    icon: "key",
    name: "SSH",
    line: "ssh, scp and rsync to every machine, with no keys to copy.",
    command: "pb ssh studio",
    group: "Work on your machines",
  },
  {
    slug: "files",
    icon: "paper-plane-tilt",
    name: "Files",
    line: "Send files to any machine. They land in its Inbox.",
    command: "pb send ./build.tar.gz --to studio",
    group: "Work on your machines",
  },
  {
    slug: "private-access",
    icon: "lock-key",
    name: "Private access",
    line: "An HTTPS address for every port on every machine, just for you.",
    command: "pb resolve studio",
    group: "Work on your machines",
  },
  {
    slug: "recordings",
    icon: "record",
    name: "Recordings",
    line: "Record a terminal session and play it back later.",
    command: "pb recordings play rec_abc123",
    group: "Work on your machines",
  },
  {
    slug: "previews",
    icon: "globe-simple",
    name: "Previews",
    line: "Put localhost on a public link in one command.",
    command: "pb preview 3000",
    group: "Put things online",
  },
  {
    slug: "tunnels",
    icon: "plugs-connected",
    name: "Tunnels and domains",
    line: "A permanent public address for a service, on your own domain.",
    command: "pb tunnel create api --port 8080",
    group: "Put things online",
  },
  {
    slug: "self-hosting",
    icon: "hard-drives",
    name: "Self-hosting",
    line: "Run your own relay and tunnel server.",
    command: "pbh status",
    group: "Put things online",
  },
  {
    slug: "env",
    icon: "vault",
    name: "ENV",
    line: "Secrets on every machine, encrypted so only you can read them.",
    command: "pb env run -- npm test",
    group: "Set up and share",
  },
  {
    slug: "config-sync",
    icon: "arrows-clockwise",
    name: "Config sync",
    line: "The same dotfiles on every machine, from your own Git repository.",
    command: "pb config sync apply",
    group: "Set up and share",
  },
  {
    slug: "recipes",
    icon: "list-checks",
    name: "Recipes",
    line: "Set up one machine or hundreds with one command, ready from the start.",
    command: "pb recipe create -f ci.toml",
    group: "Set up and share",
  },
  {
    slug: "teams",
    icon: "users-three",
    name: "Teams and access",
    line: "Share machines with your team, one permission at a time.",
    command: "pb policy explain",
    group: "Set up and share",
  },
  {
    slug: "console",
    icon: "browser",
    name: "Web console",
    line: "All your machines, terminals and links in a browser tab.",
    command: "pb machine add",
    group: "Set up and share",
  },
]

export const featureGroups = ["Work on your machines", "Put things online", "Set up and share"] as const
export const feature = (slug: string) => {
  const found = features.find((item) => item.slug === slug)
  if (!found) throw new Error(`features: unknown feature ${slug}`)
  return found
}

export const useCases = [
  {
    slug: "ai-agents",
    name: "AI agents",
    line: "Your machines, with only the access you allow.",
  },
  {
    slug: "teams",
    name: "Teams",
    line: "Shared machines, team secrets and an audit log.",
  },
  {
    slug: "homelab",
    name: "Home lab",
    line: "Every app at home by a short name.",
  },
] as const
