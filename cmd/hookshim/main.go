// Command hookshim builds the pre-receive and post-receive git hook shims.
//
// The binary name decides the verb: installed as hooks/pre-receive it execs
// `gitd pre-receive --config /etc/gitd/gitd.yaml`; as hooks/post-receive it
// execs `gitd notify --config /etc/gitd/gitd.yaml`. stdio and the exit code
// pass through untouched (R7-Q10, R11-Q7).
package main

import (
	"os"

	"github.com/ChronicCmposer/gitd/internal/hookshim"
)

func main() {
	os.Exit(hookshim.Run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}
