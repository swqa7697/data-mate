// Command data-mate is the checkout-local Data Mate executable.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/swqa7697/data-mate/internal/cli"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database/postgres"
)

var version = "development"
var revision = "unknown"
var dirty = "unknown"
var environment = "development"

func main() {
	syscall.Umask(0077)
	postgres.SanitizeEnvironment()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, cli.Build{Version: version, Revision: revision, Dirty: dirty, Environment: config.Environment(environment)})
	stop()
	os.Exit(code)
}
