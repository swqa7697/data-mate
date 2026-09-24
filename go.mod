module github.com/swqa7697/data-mate

go 1.27.0

toolchain go1.27.1

require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c
	github.com/google/jsonschema-go v0.4.3
	github.com/jackc/pgx/v5 v5.11.0
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/muesli/cancelreader v0.2.2
	github.com/pganalyze/pg_query_go/v6 v6.2.2
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.9
	github.com/tink-crypto/tink-go/v2 v2.8.0
	golang.org/x/crypto v0.57.0
	golang.org/x/net v0.59.0
	golang.org/x/sys v0.48.0
	golang.org/x/term v0.46.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/google/renameio/v2 v2.0.2 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	golang.org/x/tools v0.49.0 // indirect
	honnef.co/go/tools v0.8.1 // indirect
	mvdan.cc/editorconfig v0.3.0 // indirect
	mvdan.cc/sh/v3 v3.14.1 // indirect
)

tool (
	honnef.co/go/tools/cmd/staticcheck
	mvdan.cc/sh/v3/cmd/shfmt
)
