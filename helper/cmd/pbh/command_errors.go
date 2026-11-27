package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// usageExitCode is what a mistyped command line exits with, as pb does.
const usageExitCode = 2

// debugVariable set to 1 prints the structured diagnostic record on a command run by hand.
const debugVariable = "PAPERBOAT_DEBUG"

// usageError is a command line pbh cannot act on: the operator's typo, not a service failure. It is printed as it
// stands and never reported.
type usageError struct{ message commandError }

func (e usageError) Error() string { return e.message.Error() }
func (e usageError) Unwrap() error { return e.message }

func usage(message string) error { return usageError{commandError(message)} }

// parseFlags parses a command's flags. done means the command stops here: --help was printed, or err says what is
// wrong with the command line.
func parseFlags(fs *flag.FlagSet, args []string, out io.Writer) (done bool, err error) {
	fs.SetOutput(io.Discard)
	err = fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		printHelp(out, fs.Name(), fs)
		return true, nil
	}
	if err != nil {
		message := err.Error()
		if name, ok := strings.CutPrefix(message, "flag provided but not defined: -"); ok {
			message = "unknown flag --" + name
		}
		return true, usage(fmt.Sprintf("%s: %s; run pbh %s --help", fs.Name(), message, fs.Name()))
	}
	return false, nil
}

// serviceCommand reports whether systemd runs this command, so its stderr is the journal: the log that keeps the
// structured diagnostic records. Every other command writes to the operator's terminal.
func serviceCommand(args []string) bool {
	return len(args) > 0 && (args[0] == "serve" || args[0] == "update-apply")
}

// elevatedExit is the exit status of pbh re-run with sudo. That process (or sudo) already said what happened.
type elevatedExit int

func (e elevatedExit) Error() string {
	return fmt.Sprintf("elevated pbh exited with status %d", int(e))
}

func elevated(ctx context.Context, args []string, out io.Writer) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	command := append([]string{binary}, args...)
	if os.Getenv(debugVariable) == "1" { // sudo resets the environment
		command = append([]string{debugVariable + "=1"}, command...)
	}
	cmd := exec.CommandContext(ctx, "sudo", command...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return elevatedExit(exit.ExitCode())
	}
	return err
}
