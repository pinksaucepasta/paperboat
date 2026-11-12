package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/teambucket"
	"github.com/spf13/cobra"
)

// teamBucketCommand declares `pb team bucket`: the team's own S3-compatible bucket and what it is used for.
func teamBucketCommand() *cobra.Command {
	root := &cobra.Command{Use: "bucket", Short: "The team's own S3-compatible bucket (owners and admins)", Long: "A team connects its own S3-compatible bucket and its machines copy closed terminal recordings there, so a recording replays when its machine is offline. Paperboat stores the access key encrypted and only signs short-lived links; everything in the bucket is encrypted before it leaves the machine, and the bucket's own security is the team's. To play a recording in the browser, the bucket's CORS settings must allow GET from the console's address.", Args: commandArgs(cobra.NoArgs), RunE: func(c *cobra.Command, _ []string) error { return c.Help() }}
	get := &cobra.Command{Use: "get <team>", Short: "Show the team's bucket and its uses", Long: "Show the team's bucket: its endpoint, region, bucket name, folder prefix, access key ID and when Paperboat last wrote, read and deleted a check object in it. The secret access key is stored encrypted and is never shown, by this command or anywhere else.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		config, err := client.TeamBucket(c.Context(), args[0])
		if err != nil {
			return err
		}
		return printBucket(c.OutOrStdout(), config, jsonRequested(c))
	}}
	get.Flags().Bool("json", false, "print JSON")
	set := &cobra.Command{Use: "set <team>", Short: "Connect or change the team's bucket", Long: "Save the bucket after Paperboat writes, reads back and deletes a small check object in it; a failing step is named and nothing is saved. Give the secret access key on standard input (--secret-access-key-stdin), never as an argument; leave it out to keep the stored key while changing another field. For browser replay, the bucket's CORS settings must allow GET from the console's address.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		in, err := bucketInput(c)
		if err != nil {
			return err
		}
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		config, err := client.SetTeamBucket(c.Context(), args[0], in)
		if err != nil {
			return err
		}
		return printBucket(c.OutOrStdout(), config, jsonRequested(c))
	}}
	for _, flag := range []struct{ name, help string }{{"endpoint", "https address of the S3 service, without a path"}, {"region", "region of the bucket"}, {"bucket", "bucket name"}, {"prefix", "optional folder inside the bucket"}, {"access-key-id", "access key ID"}} {
		set.Flags().String(flag.name, "", flag.help)
	}
	set.Flags().Bool("path-style", false, "address the bucket in the URL path (MinIO and some other services)")
	set.Flags().Bool("secret-access-key-stdin", false, "read the secret access key from standard input")
	set.Flags().Bool("json", false, "print JSON")
	remove := &cobra.Command{Use: "remove <team>", Short: "Disconnect the team's bucket", Long: "Disconnect the team's bucket. Paperboat forgets the stored access key and stops giving machines and browsers links to it. What the bucket already holds stays there for the team to manage, and terminal recordings stay on their machines, where they can still be replayed while the machine is online.", Args: commandArgs(cobra.ExactArgs(1)), RunE: func(c *cobra.Command, args []string) error {
		client, err := backendForCommand(c)
		if err != nil {
			return err
		}
		if err := client.RemoveTeamBucket(c.Context(), args[0]); err != nil {
			return err
		}
		if jsonRequested(c) {
			return json.NewEncoder(c.OutOrStdout()).Encode(map[string]bool{"removed": true})
		}
		_, err = fmt.Fprintln(c.OutOrStdout(), "The team's bucket is disconnected.")
		return err
	}}
	remove.Flags().Bool("json", false, "print JSON")
	root.AddCommand(get, set, remove)
	return root
}

func jsonRequested(c *cobra.Command) bool {
	value, _ := c.Flags().GetBool("json")
	return value
}

func bucketInput(c *cobra.Command) (teambucket.Input, error) {
	flag := func(name string) string { value, _ := c.Flags().GetString(name); return strings.TrimSpace(value) }
	in := teambucket.Input{Endpoint: flag("endpoint"), Region: flag("region"), Bucket: flag("bucket"), Prefix: flag("prefix"), AccessKeyID: flag("access-key-id")}
	in.PathStyle, _ = c.Flags().GetBool("path-style")
	if stdin, _ := c.Flags().GetBool("secret-access-key-stdin"); stdin {
		line, err := bufio.NewReader(io.LimitReader(c.InOrStdin(), 1024)).ReadString('\n')
		if err != nil && err != io.EOF {
			return in, localArgumentError("the secret access key could not be read from standard input")
		}
		in.SecretAccessKey = strings.TrimSpace(line)
	}
	if err := in.Validate(); err != nil {
		return in, localArgumentError(strings.TrimPrefix(err.Error(), teambucket.ErrInvalid.Error()+": "))
	}
	return in, nil
}

func printBucket(w io.Writer, config teambucket.Config, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(config)
	}
	prefix := config.Prefix
	if prefix == "" {
		prefix = "(bucket root)"
	}
	_, err := fmt.Fprintf(w, "Bucket: %s on %s (%s)\nPrefix: %s\nAccess key ID: %s\nChecked: %s\n",
		config.Bucket, config.Endpoint, config.Region, prefix, config.AccessKeyID, config.CheckedAt.Local().Format(time.RFC3339))
	return err
}
