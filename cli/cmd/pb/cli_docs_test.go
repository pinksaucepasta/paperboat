package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

var updateCLIReference = flag.Bool("update-cli-docs", false, "regenerate the checked-in CLI reference and man pages")

// Generate from the production command tree without executing commands, loading
// preferences, accessing credentials, or contacting a daemon or server.
func TestCLIReference(t *testing.T) {
	root := newRootCommand()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	date := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	files := map[string][]byte{}
	var index strings.Builder
	index.WriteString("# pb command reference\n\nGenerated from the public CLI command definitions. Do not edit individual pages; run `make cli-docs` after updating command help.\n\nRead the [CLI guide](../cli.md) for interactive use, scripting, installation of man pages, and recovery.\n\n| Command | Description |\n| --- | --- |\n")
	var visit func(*cobra.Command)
	visit = func(c *cobra.Command) {
		if c != root && (!c.IsAvailableCommand() || c.IsAdditionalHelpTopicCommand()) {
			return
		}
		c.DisableAutoGenTag = true
		c.InitDefaultHelpFlag()
		// An inherited --json flag does not mean a raw/interactive command can
		// encode its payload. Mirror prepareJSONCommand's capability boundary.
		if c.Long == "" {
			c.Long = c.Short
		}
		switch {
		case c == root:
		case c.LocalNonPersistentFlags().Lookup("json") != nil:
			c.Long += "\n\nJSON output is supported with --json."
		case c.HasSubCommands() && c.Name() != "ssh":
			c.Long += "\n\nWith --json, this command group lists its available commands."
		default:
			c.Long += "\n\nThis command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help."
		}
		name := strings.ReplaceAll(c.CommandPath(), " ", "-")
		var man, markdown bytes.Buffer
		if err := doc.GenMan(c, &doc.GenManHeader{Section: "1", Date: &date, Source: "Paperboat", Manual: "Paperboat CLI"}, &man); err != nil {
			t.Fatal(err)
		}
		if err := doc.GenMarkdown(c, &markdown); err != nil {
			t.Fatal(err)
		}
		files["man/man1/"+name+".1"] = man.Bytes()
		files["cli/"+strings.ReplaceAll(c.CommandPath(), " ", "_")+".md"] = markdown.Bytes()
		fmt.Fprintf(&index, "| [%s](%s.md) | %s |\n", c.CommandPath(), strings.ReplaceAll(c.CommandPath(), " ", "_"), strings.ReplaceAll(c.Short, "|", "\\|"))
		for _, child := range c.Commands() {
			visit(child)
		}
	}
	visit(root)
	t.Logf("documented %d public commands in both formats", len(files)/2)
	files["cli/README.md"] = []byte(index.String())
	base := filepath.Join("..", "..", "docs")
	for name, data := range files {
		path := filepath.Join(base, filepath.FromSlash(name))
		if *updateCLIReference {
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0644); err != nil {
				t.Fatal(err)
			}
		} else if existing, err := os.ReadFile(path); err != nil || !bytes.Equal(existing, data) {
			t.Errorf("%s is missing or stale; run make cli-docs", name)
		}
	}
	// Remove obsolete generated pages when commands disappear; never touch the
	// handwritten guide or other documentation directories.
	for _, pattern := range []string{"cli/*.md", "man/man1/*.1"} {
		paths, err := filepath.Glob(filepath.Join(base, filepath.FromSlash(pattern)))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			rel, err := filepath.Rel(base, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := files[filepath.ToSlash(rel)]; exists {
				continue
			}
			if *updateCLIReference {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Errorf("obsolete generated page %s; run make cli-docs", rel)
			}
		}
	}
}
