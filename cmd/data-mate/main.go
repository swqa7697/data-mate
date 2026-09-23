// Command data-mate is the checkout-local Data Mate executable.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/swqa7697/data-mate/internal/cli"
	"github.com/swqa7697/data-mate/internal/database/postgres"
)

var version = "development"
var revision = "unknown"
var dirty = "unknown"

func main() {
	postgres.SanitizeEnvironment()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, cli.Build{Version: version, Revision: revision, Dirty: dirty})
	stop()
	os.Exit(code)
}
