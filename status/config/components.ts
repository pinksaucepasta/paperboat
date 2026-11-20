// The status page's components: which units each has and which vantage points report them. Plain TypeScript so every bundler reads it.
export default {
  "vantages": [
    "vantage-a",
    "vantage-b",
    "cloudflare"
  ],
  "components": [
    {
      "id": "api",
      "name": "Sign-in & API",
      "note": "New connections and sign-in. Connected machines keep working.",
      "units": [
        {
          "id": "api_health",
          "name": "Service ready",
          "vantages": [
            "vantage-a",
            "vantage-b",
            "cloudflare"
          ],
          "slowMs": 3000
        },
        {
          "id": "api_read",
          "name": "Public read",
          "vantages": [
            "vantage-a",
            "vantage-b",
            "cloudflare"
          ],
          "slowMs": 3000
        },
        {
          "id": "api_auth",
          "name": "Signed-in read",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 8000
        }
      ]
    },
    {
      "id": "coordination",
      "name": "Machine connections",
      "note": "New terminal, exec and ssh sessions. Open ones keep running.",
      "units": [
        {
          "id": "exec",
          "name": "Exec",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 15000
        },
        {
          "id": "ssh",
          "name": "SSH",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 15000
        }
      ]
    },
    {
      "id": "relays",
      "name": "Relays",
      "note": "Only used when machines can't connect directly.",
      "units": [
        {
          "id": "relay_india_1",
          "name": "India \u00b7 relay 1",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 3000
        },
        {
          "id": "relay_india_2",
          "name": "India \u00b7 relay 2",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 3000
        }
      ]
    },
    {
      "id": "edges",
      "name": "Edges",
      "note": "Previews, tunnels and the browser terminal.",
      "units": [
        {
          "id": "edge_india_1",
          "name": "India \u00b7 edge 1",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 5000
        },
        {
          "id": "edge_india_2",
          "name": "India \u00b7 edge 2",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 5000
        },
        {
          "id": "previews",
          "name": "Previews",
          "vantages": [
            "vantage-a",
            "vantage-b"
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
            "vantage-a",
            "vantage-b"
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
            "vantage-a",
            "vantage-b",
            "cloudflare"
          ],
          "slowMs": 5000
        }
      ]
    },
    {
      "id": "updates",
      "name": "Updates & releases",
      "note": "Downloading new versions.",
      "units": [
        {
          "id": "release_meta",
          "name": "Release metadata",
          "vantages": [
            "vantage-a",
            "vantage-b",
            "cloudflare"
          ],
          "slowMs": 5000
        }
      ]
    },
    {
      "id": "website",
      "name": "Website",
      "note": "The website and docs.",
      "units": [
        {
          "id": "website_home",
          "name": "Home page",
          "vantages": [
            "vantage-a",
            "vantage-b",
            "cloudflare"
          ],
          "slowMs": 5000
        }
      ]
    }
  ]
}
