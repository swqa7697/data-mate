package service

import (
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/mysql"
	"github.com/swqa7697/data-mate/internal/database/pool"
	"github.com/swqa7697/data-mate/internal/database/postgres"
)

// NewDriver composes every supported database driver over one shared registry,
// so admission, pool and cursor bounds span drivers. It contacts no database.
func NewDriver() (database.Driver, error) {
	r, err := pool.New(pool.Options{})
	if err != nil {
		return nil, err
	}
	return database.NewRouter(r, map[string]database.Operations{
		"postgres": postgres.New(r).Operations(),
		"mysql":    mysql.New(r, mysql.MySQL).Operations(),
		"mariadb":  mysql.New(r, mysql.MariaDB).Operations(),
	}), nil
}
