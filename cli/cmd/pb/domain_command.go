package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

// pb domain: domains added once to the workspace, then used by any tunnel or preview hostname under them
// (`pb tunnel create --domain app.example.com`, `pb preview --domain app.example.com`).
func domainCobraCommand() *cobra.Command {
	root := &cobra.Command{Use: "domain", Short: "Add your own domains to the workspace", Long: `Add a domain you control (example.com or dev.example.com) once. After its DNS records
are in place and it shows verified, any name one level under it (app.example.com) can be used by
tunnels and previews with --domain, with HTTPS from one shared certificate.`, Args: commandArgs(cobra.NoArgs)}
	root.AddCommand(domainAddCommandV1(), domainListCommandV1(), domainShowCommandV1(), domainVerifyCommandV1(), domainRemoveCommandV1())
	return root
}

func domainAddCommandV1() *cobra.Command {
	c := &cobra.Command{Use: "add <domain>", Short: "Add a domain and show its DNS records", Example: "  pb domain add dev.example.com", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		domain, err := client.AddWorkspaceDomain(c.Context(), strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}
		return writeWorkspaceDomain(c, domain, fmt.Sprintf("Added %s. Add these records at your DNS provider, then run `pb domain verify %s`.", domain.Hostname, domain.Hostname))
	}}
	c.Flags().Bool("json", false, "print JSON")
	return c
}

func domainListCommandV1() *cobra.Command {
	c := &cobra.Command{Use: "list", Short: "List the workspace's domains", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		domains, err := client.ListWorkspaceDomains(c.Context())
		if err != nil {
			return err
		}
		if asJSON, _ := c.Flags().GetBool("json"); asJSON {
			return writeCLIJSON(c.OutOrStdout(), map[string]any{"items": domains})
		}
		if len(domains) == 0 {
			_, err := fmt.Fprintln(c.OutOrStdout(), "No domains yet. Add one with `pb domain add <domain>` to use your own names for tunnels and previews.")
			return err
		}
		for _, d := range domains {
			if _, err := fmt.Fprintf(c.OutOrStdout(), "%s\t%s\tcertificate %s\t%d in use\n", d.Hostname, domainStateText(d.State), d.Certificate.State, len(d.UsedBy)); err != nil {
				return err
			}
		}
		return nil
	}}
	c.Flags().Bool("json", false, "print JSON")
	return c
}

func domainShowCommandV1() *cobra.Command {
	c := &cobra.Command{Use: "show <domain>", Short: "Show a domain's DNS records, status and uses", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		domain, err := resolveWorkspaceDomain(c.Context(), client, args[0])
		if err != nil {
			return err
		}
		return writeWorkspaceDomain(c, domain, "")
	}}
	c.Flags().Bool("json", false, "print JSON")
	return c
}

func domainVerifyCommandV1() *cobra.Command {
	c := &cobra.Command{Use: "verify <domain>", Short: "Check a domain's DNS now and wait until it is verified", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		domain, err := resolveWorkspaceDomain(c.Context(), client, args[0])
		if err != nil {
			return err
		}
		if domain, err = client.CheckWorkspaceDomain(c.Context(), domain.ID); err != nil {
			return err
		}
		timeout, _ := c.Flags().GetDuration("timeout")
		ctx, cancel := context.WithTimeout(c.Context(), timeout)
		defer cancel()
		// New records can take minutes to reach the resolvers: keep waiting through failed checks
		// (the server keeps checking) until verified with a certificate, or the timeout says why not.
		for domain.State != "verified" || domain.Certificate.State == "pending" {
			select {
			case <-ctx.Done():
				if err := writeWorkspaceDomain(c, domain, "Not verified yet. DNS changes can take a few minutes; run this again later."); err != nil {
					return err
				}
				return domainRefusal(fmt.Sprintf("%s is not verified with a certificate within --timeout; check its DNS records with `pb domain show %s`", domain.Hostname, domain.Hostname))
			case <-time.After(2 * time.Second):
			}
			if domain, err = client.GetWorkspaceDomain(c.Context(), domain.ID); err != nil {
				return err
			}
		}
		if domain.Certificate.State == "failed" {
			if err := writeWorkspaceDomain(c, domain, "Verified, but the certificate could not be issued yet."); err != nil {
				return err
			}
			return domainRefusal(fmt.Sprintf("%s is verified but its certificate could not be issued yet; Paperboat retries, see `pb domain show %s`", domain.Hostname, domain.Hostname))
		}
		return writeWorkspaceDomain(c, domain, fmt.Sprintf("%s is verified. Use any name under it: pb tunnel create web --domain app.%s", domain.Hostname, domain.Hostname))
	}}
	c.Flags().Duration("timeout", 2*time.Minute, "how long to wait for verification and the certificate")
	c.Flags().Bool("json", false, "print JSON")
	return c
}

func domainRemoveCommandV1() *cobra.Command {
	c := &cobra.Command{Use: "remove <domain>", Short: "Remove a domain no tunnel or preview uses", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		domain, err := resolveWorkspaceDomain(c.Context(), client, args[0])
		if err != nil {
			return err
		}
		if len(domain.UsedBy) > 0 {
			return workspaceDomainInUse(domain)
		}
		if err := confirmMutation(c, "domain-remove:"+domain.ID, fmt.Sprintf("Remove %s from this workspace? Your DNS records stay; remove them at your provider.", domain.Hostname)); err != nil {
			return err
		}
		if err := client.RemoveWorkspaceDomain(c.Context(), domain.ID); err != nil {
			return err
		}
		if asJSON, _ := c.Flags().GetBool("json"); asJSON {
			return writeCLIJSON(c.OutOrStdout(), map[string]any{"removed": domain.ID, "hostname": domain.Hostname})
		}
		_, err = fmt.Fprintf(c.OutOrStdout(), "Removed %s. Its DNS records stay at your provider.\n", domain.Hostname)
		return err
	}}
	c.Flags().String("confirm", "", "six-character confirmation code from the preview")
	c.Flags().Bool("json", false, "print JSON")
	return c
}

func workspaceDomainInUse(domain api.WorkspaceDomain) error {
	uses := make([]string, 0, len(domain.UsedBy))
	for _, u := range domain.UsedBy {
		uses = append(uses, fmt.Sprintf("%s (%s %s)", u.Hostname, u.Kind, u.Name))
	}
	return domainRefusal(fmt.Sprintf("%s is used by %s; remove those hostnames first (pb tunnel domain remove <tunnel> <hostname>)", domain.Hostname, strings.Join(uses, ", ")))
}

// resolveWorkspaceDomain finds a domain of the active workspace by hostname or id.
func resolveWorkspaceDomain(ctx context.Context, client *api.Client, value string) (api.WorkspaceDomain, error) {
	value = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
	domains, err := client.ListWorkspaceDomains(ctx)
	if err != nil {
		return api.WorkspaceDomain{}, err
	}
	for _, d := range domains {
		if d.ID == value || d.Hostname == value {
			return d, nil
		}
	}
	return api.WorkspaceDomain{}, domainRefusal("no domain " + value + " in this workspace; see `pb domain list`")
}

// domainRefusal is a state the user fixes; its message reaches human and --json output as written.
func domainRefusal(message string) error {
	return usageError{err: errors.New(message), publicMessage: message, refusal: true}
}

func domainStateText(state string) string {
	switch state {
	case "verified":
		return "verified"
	case "failed":
		return "not verified (checking slowly; run pb domain verify)"
	default:
		return "waiting for DNS"
	}
}

func writeWorkspaceDomain(c *cobra.Command, d api.WorkspaceDomain, note string) error {
	if asJSON, _ := c.Flags().GetBool("json"); asJSON {
		return writeCLIJSON(c.OutOrStdout(), d)
	}
	w := c.OutOrStdout()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\t%s\n", d.Hostname, domainStateText(d.State))
	if d.DNSFailure != nil {
		fmt.Fprintf(&b, "  %s\n", d.DNSFailure.Message)
	}
	b.WriteString("DNS records:\n")
	for _, r := range d.Records {
		need := "required"
		switch r.Purpose {
		case "verification":
			need = "required: proves you control the domain"
		case "routing":
			need = "required: sends names under it to Paperboat"
		case "apex_routing":
			need = "optional: only to serve " + d.Hostname + " itself"
		}
		fmt.Fprintf(&b, "  %s\t%s\t%s\t(%s)\n", r.Name, r.Type, r.Value, need)
	}
	fmt.Fprintf(&b, "Certificate: %s", d.Certificate.State)
	if d.Certificate.ExpiresAt != "" {
		fmt.Fprintf(&b, " until %s", d.Certificate.ExpiresAt)
	}
	b.WriteString("\n")
	if d.Certificate.Failure != nil {
		fmt.Fprintf(&b, "  %s\n", d.Certificate.Failure.Message)
	}
	if len(d.UsedBy) > 0 {
		b.WriteString("Used by:\n")
		for _, u := range d.UsedBy {
			fmt.Fprintf(&b, "  https://%s\t%s %s\n", u.Hostname, u.Kind, u.Name)
		}
	}
	if note != "" {
		b.WriteString(note + "\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
