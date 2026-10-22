## pb team sso setup

Open the WorkOS Admin Portal to verify a domain and connect your identity provider

### Synopsis

Print, and open in a browser where possible, a short-lived WorkOS Admin Portal link for one setup step. Use --step domain to verify a domain with a DNS TXT record, then --step sso to connect the identity provider with the guide for your provider. The team needs enough seats; the first run creates its WorkOS organization.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb team sso setup <team> [flags]
```

### Options

```
  -h, --help          help for setup
      --step string   domain (verify a domain) or sso (connect the identity provider) (default "sso")
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team sso](pb_team_sso.md)	 - Single sign-on for a team (owners and admins)

