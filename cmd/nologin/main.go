// Command nologin is the nologin shell for the sshd and git users in the
// from-scratch image (R2-Q3: git user shell nologin; the image has no
// /sbin/nologin or busybox). It prints a short message and exits 1.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "This account is currently not available.")
	os.Exit(1)
}
