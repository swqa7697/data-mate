package service

import (
	"context"
	"github.com/swqa7697/data-mate/internal/config"
	"path/filepath"
	"slices"
	"strconv"
)

func itoa(n int) string { return strconv.Itoa(n) }

type job struct {
	Present bool
	PID     int
	Path    string
	Args    []string
}

// launchManager is the external process boundary; fakes cannot replace runtime
// ownership, socket verification, vault validation or state locking.
type launchManager interface {
	Inspect(context.Context, config.Root) (job, error)
	Bootstrap(context.Context, config.Root) error
	Bootout(context.Context, config.Root) error
}

func matching(j job, r record) bool {
	return j.Present && j.Path == filepath.Join(r.Identity.Root, config.ServiceFile()) && slices.Equal(j.Args, r.args())
}
