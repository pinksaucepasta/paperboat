// The status page's components: which units each has and which vantage points report them. Plain TypeScript so every bundler reads it.
export default {
  "vantages": [
    "vantage-a",
    "vantage-b"
  ],
  "components": [
    {
      "id": "api",
      "name": "Sign-in & API",
      "note": "Staging control plane.",
      "units": [
        {
          "id": "api_health",
          "name": "Service ready",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 3000
        },
        {
          "id": "api_read",
          "name": "Public read",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 3000
        }
      ]
    },
    {
      "id": "relays",
      "name": "Relays",
      "note": "Staging relay.",
      "units": [
        {
          "id": "relay_anvitstg",
          "name": "Staging relay",
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
      "note": "Staging edge.",
      "units": [
        {
          "id": "edge_anvitstg",
          "name": "Staging edge",
          "vantages": [
            "vantage-a",
            "vantage-b"
          ],
          "slowMs": 3000
        }
      ]
    }
  ]
}
