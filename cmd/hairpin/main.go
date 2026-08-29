// Command hairpin runs the hairpin server: a task-submission API and
// web UI on one side, and the stirrup gRPC control plane on the other.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hairpin:", err)
		os.Exit(1)
	}
}
