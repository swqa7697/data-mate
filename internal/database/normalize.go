package database

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/transport"
)

// Normalize revalidates an access snapshot through the strict profile decoder and
// returns the canonical profile and its revision. Drivers call it at every entry
// point; it never caches access or contacts a database.
func Normalize(a Access) (Access, config.Revision, error) {
	b, err := json.Marshal(config.Profiles{Version: 1, Connections: []config.Profile{a.Profile}})
	if err != nil {
		return a, "", Fail(contracts.ConfigInvalid, "invalid profile", false)
	}
	p, rev, err := config.DecodeProfiles(bytes.NewReader(b))
	if err != nil {
		return a, "", Fail(contracts.ConfigInvalid, "invalid profile", false)
	}
	a.Profile = p.Connections[0]
	host := a.Profile.Connection.Host
	if len(host) > 253 || (net.ParseIP(host) == nil && strings.ContainsAny(host, "/\\:@, \t\r\n")) {
		return a, "", Fail(contracts.ConfigInvalid, "host must be one TCP hostname or IP address", false)
	}
	if len(a.Password()) > 128<<10 || strings.ContainsRune(a.Password(), 0) {
		return a, "", Fail(contracts.ConfigInvalid, "invalid credential", false)
	}
	return a, rev, nil
}

// Timeout is the profile's operation budget. Callers apply it to a context
// derived from the caller's own, so earlier deadlines still win.
func Timeout(a Access) time.Duration {
	return time.Duration(a.Profile.Limits.QueryTimeoutMS) * time.Millisecond
}

// CommonError translates errors whose public meaning is driver-independent:
// typed boundary errors, transport sentinels and context termination. It
// returns nil when err needs driver-specific classification.
func CommonError(err error) error {
	var known *Error
	if errors.As(err, &known) {
		return known
	}
	for _, safe := range []error{transport.ErrUnknownHost, transport.ErrChangedHost, transport.ErrKnownHosts} {
		if errors.Is(err, safe) {
			return Fail(contracts.ConnectFailed, safe.Error(), false)
		}
	}
	if errors.Is(err, transport.ErrConfiguration) {
		return Fail(contracts.ConfigInvalid, "invalid transport configuration or credentials", false)
	}
	if errors.Is(err, context.Canceled) {
		return Fail(contracts.Cancelled, "database operation canceled", true)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Fail(contracts.QueryTimeout, "database operation timed out", true)
	}
	return nil
}

// SafeSQLState returns a well-formed SQLSTATE, the only upstream diagnostic
// field that crosses the public boundary, or "" for anything else.
func SafeSQLState(code string) string {
	if len(code) != 5 {
		return ""
	}
	for _, c := range code {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z') {
			return ""
		}
	}
	return code
}
