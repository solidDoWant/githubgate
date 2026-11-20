// Command fakegithub runs the fake GitHub for e2e tests: the API and git on
// one port, the test control API on another.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/solidDoWant/githubgate/test/fakegithub"
)

func main() {
	apiAddr := flag.String("api-address", ":8080", "address of the GitHub API and git")
	controlAddr := flag.String("control-address", ":8081", "address of the test control API")
	gitRoot := flag.String("git-root", "", "directory for bare repositories (default: a temporary directory)")

	flag.Parse()

	server, err := fakegithub.New(*gitRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakegithub:", err)
		os.Exit(1)
	}

	errs := make(chan error, 2)

	for addr, handler := range map[string]http.Handler{*apiAddr: server.Handler(), *controlAddr: server.ControlHandler()} {
		httpServer := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

		go func() { errs <- httpServer.ListenAndServe() }()
	}

	fmt.Fprintln(os.Stderr, "fakegithub:", <-errs)
	os.Exit(1)
}
