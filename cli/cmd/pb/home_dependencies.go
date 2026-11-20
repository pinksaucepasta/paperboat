package main

import (
	"context"

	"github.com/spf13/cobra"
)

// homeDependencies are the edges of the interactive home screen that tests replace.
type homeDependencies struct {
	newRoot  func() *cobra.Command                      // a fresh command tree for each action the menu runs
	showText func(*cobra.Command, string, string) error // the full-screen viewer for a result
}

func defaultHomeDependencies() homeDependencies {
	return homeDependencies{newRoot: newRootCommand, showText: renderHomeText}
}

func homeDependenciesOf(ctx context.Context) homeDependencies {
	return dependenciesIn(ctx, defaultHomeDependencies)
}

// showHomeText shows a result in the home screen's viewer.
func showHomeText(command *cobra.Command, title, content string) error {
	return homeDependenciesOf(command.Context()).showText(command, title, content)
}
