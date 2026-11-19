// Command githubgate is the githubgate server. For now it only prints its
// version; the server is built on top of this entry point.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "githubgate:", err)
		os.Exit(1)
	}
}

func run(stdout io.Writer) error {
	_, err := fmt.Fprintf(stdout, "githubgate %s\n", version)

	return err
}
