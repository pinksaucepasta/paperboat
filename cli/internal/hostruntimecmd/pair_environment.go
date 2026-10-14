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
