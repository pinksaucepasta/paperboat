package hostruntimecmd

import (
	"fmt"
	"io"
)

// ENV delivery is independent of accepted machine pairing. The runtime keeps
// retrying recipient registration and obtains fresh layers on every launch.
func reportPairEnvironment(output io.Writer, environmentErr error, readyMessage string) error {
	if environmentErr != nil {
		_, err := fmt.Fprintln(output, "Paperboat is ready; managed environment variables are pending: unlock your ENV vault to deliver them")
		return err
	}
	_, err := fmt.Fprintln(output, readyMessage)
	return err
}

// pairedReadyMessage is the settled pairing message; bootstrap names the machine and its next step in it.
const pairedReadyMessage = "Paperboat machine runtime is ready."

// namedReadyMessage names the machine the server enrolled (its -N name after a collision) and the next step.
func namedReadyMessage(paired, alias string) string {
	if paired != pairedReadyMessage || alias == "" {
		return paired
	}
	return "Ready: this machine is " + alias + ". Open it from any device with: pb connect " + alias
}
