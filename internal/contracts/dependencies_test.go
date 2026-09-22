package contracts_test

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	pgquery "github.com/pganalyze/pg_query_go/v6"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"
	"golang.org/x/sys/unix"
)

// TestPinnedDependencies compiles native parser and every future boundary pin.
// It performs no network, terminal, agent, or credential operation.
func TestPinnedDependencies(t *testing.T) {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
		}
	}
	if _, err := pgquery.Parse("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	config, err := pgconn.ParseConfig("host=localhost user=probe dbname=probe sslmode=disable passfile=/dev/null servicefile=/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	config.MaxProtocolMessageBodyLen = 2 << 20
	_ = mcp.NewServer(&mcp.Implementation{Name: "contract-probe", Version: "test"}, nil)
	_ = huh.NewInput()
	if _, err := proxy.SOCKS5("tcp", "127.0.0.1:1", nil, proxy.Direct); err != nil {
		t.Fatal(err)
	}
	_ = ssh.ClientConfig{User: "fixture"}
	if unix.O_NOFOLLOW == 0 {
		t.Fatal("no-follow support missing")
	}
}
