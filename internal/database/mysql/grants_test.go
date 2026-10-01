package mysql

import (
	"errors"
	"strings"
	"testing"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// The audit's privilege decisions are a security boundary; the live matrix in
// the Docker fixture proves grant collection, this corpus proves classification.
func TestAuditGrants(t *testing.T) {
	accepted := [][]string{
		{"GRANT USAGE ON *.* TO `reader`@`%`", "GRANT SELECT, SHOW VIEW ON `app`.* TO `reader`@`%`"},
		{"GRANT SELECT (`id`, `name`) ON `app`.`items` TO `reader`@`%`", "GRANT EXECUTE ON PROCEDURE `app`.`p` TO `reader`@`%`"},
		{"GRANT PROCESS, REPLICATION CLIENT, SHOW DATABASES, LOCK TABLES ON *.* TO `reader`@`%`", "GRANT SHOW_ROUTINE ON *.* TO `reader`@`%`"},
		{"GRANT `analyst`@`%`,`viewer`@`%` TO `reader`@`%`", "REVOKE INSERT ON `mysql`.* FROM `reader`@`%`"},
		// MariaDB spells monitoring privileges with words and shows password hashes.
		{"GRANT BINLOG MONITOR, SLAVE MONITOR ON *.* TO `reader`@`%` IDENTIFIED BY PASSWORD '*0123456789ABCDEF0123456789ABCDEF01234567'"},
		{"GRANT USAGE ON *.* TO `reader`@`%` IDENTIFIED VIA ed25519 USING PASSWORD('secret-sentinel') REQUIRE SSL WITH MAX_QUERIES_PER_HOUR 10"},
		{"GRANT `viewer` TO `reader`@`%`", "SET DEFAULT ROLE `viewer` FOR `reader`@`%`", "GRANT SHOW CREATE ROUTINE ON `app`.* TO `viewer`"},
	}
	for _, lines := range accepted {
		if err := auditGrants(lines); err != nil {
			t.Errorf("rejected read-only grants %q: %v", lines, err)
		}
	}
	rejected := []struct{ line, reason string }{
		{"GRANT SELECT, INSERT ON `app`.* TO `reader`@`%`", "INSERT"},
		{"GRANT SELECT (`id`), UPDATE (`name`) ON `app`.`items` TO `reader`@`%`", "UPDATE"},
		{"GRANT REFERENCES (`id`) ON `app`.`items` TO `reader`@`%`", "REFERENCES"},
		{"GRANT ALL PRIVILEGES ON `app`.* TO `reader`@`%`", "ALL PRIVILEGES"},
		{"GRANT ALL ON *.* TO `reader`@`%`", "ALL"},
		{"GRANT CREATE TEMPORARY TABLES ON `app`.* TO `reader`@`%`", "CREATE TEMPORARY TABLES"},
		{"GRANT TRIGGER ON `app`.* TO `reader`@`%`", "TRIGGER"},
		{"GRANT EVENT ON `app`.* TO `reader`@`%`", "EVENT"},
		{"GRANT FILE ON *.* TO `reader`@`%`", "FILE"},
		{"GRANT SUPER ON *.* TO `reader`@`%`", "SUPER"},
		{"GRANT BACKUP_ADMIN,SHOW_ROUTINE ON *.* TO `reader`@`%`", "BACKUP_ADMIN"},
		{"GRANT REPLICATION SLAVE ON *.* TO `reader`@`%`", "REPLICATION SLAVE"},
		{"GRANT SELECT ON `app`.* TO `reader`@`%` WITH GRANT OPTION", "privilege administration"},
		{"GRANT USAGE ON *.* TO `reader`@`%` WITH MAX_USER_CONNECTIONS 2 GRANT OPTION", "privilege administration"},
		{"GRANT `writer`@`%` TO `reader`@`%` WITH ADMIN OPTION", "role administration"},
		{"GRANT PROXY ON `admin`@`%` TO `reader`@`%`", "proxy"},
		{"GRANT DELETE HISTORY ON `app`.* TO `reader`", "DELETE HISTORY"},
	}
	for _, c := range rejected {
		err := auditGrants([]string{"GRANT USAGE ON *.* TO `reader`@`%`", c.line})
		var e *database.Error
		if !errors.As(err, &e) || e.Code != contracts.ReadOnlyViolation || !strings.Contains(e.Message, c.reason) {
			t.Errorf("%q: wanted %s rejection, got %v", c.line, c.reason, err)
		}
	}
	// Shapes the parser does not recognize fail closed; secrets never surface.
	for _, line := range []string{"", "CREATE USER `x`", "GRANT SELECT TO", "GRANT ON *.* TO `r`", "GRANT SELECT /*!50000 , INSERT */ ON *.* TO `r`", "GRANT 'quoted' ON *.* TO `r`"} {
		err := auditGrants([]string{line})
		var e *database.Error
		if !errors.As(err, &e) || e.Code != contracts.ReadOnlyViolation || strings.Contains(e.Message, "secret-sentinel") {
			t.Errorf("%q: wanted fail-closed rejection, got %v", line, err)
		}
	}
}
