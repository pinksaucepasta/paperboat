// The status page's components: which units each has and which vantage points report them. Plain TypeScript so every bundler reads it.
export default {
  "vantages": [
    "oracle",
    "anvit",
    "cloudflare"
  ],
  "components": [
    {
      "id": "api",
      "name": "Sign-in & API",
      "note": "New connections, pairing and the console sign-in. Machines that are already connected keep working.",
      "units": [
        {
          "id": "api_health",
          "name": "Service ready",
          "vantages": [
            "oracle",
            "anvit",
            "cloudflare"
          ],
          "slowMs": 3000
        },
        {
          "id": "api_read",
          "name": "Public read",
          "vantages": [
            "oracle",
            "anvit",
            "cloudflare"
          ],
          "slowMs": 3000
        },
        {
          "id": "api_auth",
          "name": "Signed-in read",
          "vantages": [
            "oracle",
            "anvit"
          ],
          "slowMs": 8000
        }
      ]
    },
    {
      "id": "coordination",
      "name": "Machine connections",
      "note": "Starting new terminal, exec and ssh sessions. Sessions that are already open keep running.",
      "units": [
        {
          "id": "exec",
          "name": "Exec",
          "vantages": [
            "oracle",
            "anvit"
          ],
          "slowMs": 15000
        },
        {
          "id": "ssh",
          "name": "SSH",
          "vantages": [
            "oracle",
            "anvit"
          ],
          "slowMs": 15000
        }
      ]
    },
    {
      "id": "relays",
      "name": "Relays",
      "note": "Only machines that cannot connect directly use a relay. Everyone else is not affected.",
      "units": [
        {
          "id": "relay_india_1",
          "name": "India \u00b7 relay 1",
          "vantages": [
            "oracle",
            "anvit"
          ],
          "slowMs": 3000
        },
        {
          "id": "relay_india_2",
          "name": "India \u00b7 relay 2",
          "vantages": [
            "oracle",
            "anvit"
          ],
          "slowMs": 3000
        }
      ]
    },
    {
      "id": "edges",
      "name": "Edges",
      "note": "Previews, tunnels, the browser terminal and private-tunnel sign-in.",
      "units": [
        {
          "id": "edge_india_1",
          "name": "India \u00b7 edge 1",
          "vantages": [
            "oracle",
            "anvit"
          ],
          "slowMs": 5000
        },
        {
          "id": "edge_india_2",
          "name": "India \u00b7 edge 2",
          "vantages": [
            "oracle",
            "anvit"
          ],
          "slowMs": 5000
        },
        {
          "id": "previews",
          "name": "Previews",
          "vantages": [
            "oracle",
            "anvit"
          ],
          "slowMs": 15000
        }
      ]
    },
    {
      "id": "transfers",
      "name": "File transfers",
      "note": "Sending files between machines.",
      "units": [
        {
          "id": "send",
          "name": "Send",
          "vantages": [
            "oracle",
            "anvit"
          ],
          "slowMs": 20000
        }
      ]
    },
    {
      "id": "console",
      "name": "Console",
      "note": "The web console.",
      "units": [
        {
          "id": "console_load",
          "name": "Page load",
          "vantages": [
            "oracle",
            "anvit",
            "cloudflare"
          ],
          "slowMs": 5000
        }
      ]
    },
    {
      "id": "updates",
      "name": "Updates & releases",
      "note": "Downloading and verifying new versions.",
      "units": [
        {
          "id": "release_meta",
          "name": "Release metadata",
          "vantages": [
            "oracle",
            "anvit",
            "cloudflare"
          ],
          "slowMs": 5000
        }
      ]
    },
    {
      "id": "website",
      "name": "Website",
      "note": "The Paperboat website and docs.",
      "units": [
        {
          "id": "website_home",
          "name": "Home page",
          "vantages": [
            "oracle",
            "anvit",
            "cloudflare"
          ],
          "slowMs": 5000
        }
      ]
    }
  ]
}
