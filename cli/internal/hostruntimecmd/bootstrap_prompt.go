package hostruntimecmd

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

func promptBootstrapValue(reader *bufio.Reader, output io.Writer, label string, value *string) error {
	*value = strings.TrimSpace(*value)
	if *value != "" {
		return nil
	}
	if _, err := fmt.Fprintf(output, "%s: ", label); err != nil {
		return runtimeFailure("writing the enrollment prompt failed", err)
	}
	line, err := reader.ReadString('\n')
	if err != nil && !runtimeCausesOnly(err, io.EOF) {
		return err
	}
	*value = strings.TrimSpace(line)
	if *value == "" {
		return fmt.Errorf("%s is required", strings.ToLower(label))
	}
	return nil
}
