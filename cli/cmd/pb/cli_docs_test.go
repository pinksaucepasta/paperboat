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
	"github.com/spf13/pflag"
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
		if len(strings.Fields(c.Long)) < 25 {
			t.Errorf("%s needs a substantive command description", c.CommandPath())
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
		restore := escapeManPlaceholders(c)
		manErr := doc.GenMan(c, &doc.GenManHeader{Section: "1", Date: &date, Source: "Paperboat", Manual: "Paperboat CLI"}, &man)
		restore()
		if manErr != nil {
			t.Fatal(manErr)
		}
		if err := doc.GenMarkdown(c, &markdown); err != nil {
			t.Fatal(err)
		}
		manPage := bytes.ReplaceAll(man.Bytes(), []byte("&lt;"), []byte("<"))
		manPage = bytes.ReplaceAll(manPage, []byte("&gt;"), []byte(">"))
		if strings.Contains(c.Use, "<") && !bytes.Contains(manPage, []byte("<")) {
			t.Errorf("%s man synopsis lost an argument placeholder", c.CommandPath())
		}
		files["man/man1/"+name+".1"] = manPage
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

// Cobra's man converter treats unescaped <argument> as an HTML tag and drops
// it. Escape only for man generation; runtime help and Markdown keep the
// original human-readable spelling.
func escapeManPlaceholders(command *cobra.Command) func() {
	escape := func(value string) string {
		return strings.ReplaceAll(strings.ReplaceAll(value, "<", "&lt;"), ">", "&gt;")
	}
	use, short, long, example := command.Use, command.Short, command.Long, command.Example
	command.Use, command.Short, command.Long, command.Example = escape(use), escape(short), escape(long), escape(example)
	usages := map[*pflag.Flag]string{}
	collect := func(flag *pflag.Flag) {
		if _, seen := usages[flag]; !seen {
			usages[flag] = flag.Usage
			flag.Usage = escape(flag.Usage)
		}
	}
	command.Flags().VisitAll(collect)
	command.InheritedFlags().VisitAll(collect)
	return func() {
		command.Use, command.Short, command.Long, command.Example = use, short, long, example
		for flag, usage := range usages {
			flag.Usage = usage
		}
	}
}
