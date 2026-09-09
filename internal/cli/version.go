package cli

import (
	"io"

	"github.com/ChronicCmposer/gitd/internal/version"
)

// runVersion prints the gitd version. It accepts no arguments.
func runVersion(args []string, stdout, _ io.Writer) error {
	if len(args) > 0 {
		return errUsage("version takes no arguments")
	}
	_, err := io.WriteString(stdout, version.String()+"\n")
	return err
}
