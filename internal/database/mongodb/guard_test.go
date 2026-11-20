package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
)

// MongoDB has no read-only transaction, so the guard is part of the read-only
// boundary: writes, server-side JavaScript, internal operators and inputs the
// parser would silently change must be refused before any connection opens.
func TestDocumentInputGuard(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var accepts atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			c.Close()
		}
	}()
	d := New(fakeRegistry(t))
	a := database.NewAccess(fakeProfile(ln.Addr().(*net.TCPAddr).Port, config.DefaultLimits()), fakeSecret)
	find := func(filter, projection string) *database.FindRequest {
		req := &database.FindRequest{Database: "app", Collection: "items"}
		if filter != "" {
			req.Filter = json.RawMessage(filter)
		}
		if projection != "" {
			req.Projection = json.RawMessage(projection)
		}
		return req
	}
	hugeDecimal := `{"price":{"$numberDecimal":"1.` + strings.Repeat("0", 200_000) + `"}}`
	for _, tt := range []struct {
		name     string
		find     *database.FindRequest
		pipeline []string
		code     contracts.Code
	}{
		{"$out stage", nil, []string{`{"$match":{}}`, `{"$out":"copy"}`}, contracts.ReadOnlyViolation},
		{"$merge in a $facet pipeline", nil, []string{`{"$facet":{"a":[{"$match":{}}],"b":[{"$merge":{"into":"copy"}}]}}`}, contracts.ReadOnlyViolation},
		{"$out in a $lookup pipeline", nil, []string{`{"$lookup":{"from":"other","pipeline":[{"$out":"copy"}],"as":"o"}}`}, contracts.ReadOnlyViolation},
		{"$where filter", find(`{"$where":"this.a == 1"}`, ""), nil, contracts.QueryUnsupported},
		{"$function in a $unionWith pipeline", nil, []string{`{"$unionWith":{"coll":"other","pipeline":[{"$addFields":{"x":{"$function":{"body":"function(){return 1}","args":[],"lang":"js"}}}}]}}`}, contracts.QueryUnsupported},
		{"BSON code value in a projection", find("", `{"x":{"$code":"function(){return 1}"}}`), nil, contracts.QueryUnsupported},
		{"internal operator that evaluates a string", find("", `{"x":{"$_internalJsEmit":{"eval":"function(){emit(1,1)}","this":{}}}}`), nil, contracts.QueryUnsupported},
		{"unknown stage", nil, []string{`{"$fooBar":{}}`}, contracts.QueryUnsupported},
		{"stage with two operators", nil, []string{`{"$match":{},"$limit":1}`}, contracts.InvalidArgument},
		{"integer outside 64 bits", find(`{"_id":9223372036854775808}`, ""), nil, contracts.InvalidArgument},
		{"decimal text long enough for quadratic parsing", find(hugeDecimal, ""), nil, contracts.InvalidArgument},
		// An accepted nested pipeline reaches the server, which closes the connection.
		{"accepted nested pipeline", nil, []string{`{"$lookup":{"from":"other","pipeline":[{"$match":{"a":{"$gt":1}}}],"as":"o"}}`, `{"$facet":{"n":[{"$count":"n"}]}}`}, contracts.ConnectFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			before := accepts.Load()
			if tt.find != nil {
				_, err = d.FindDocuments(ctx, a, *tt.find)
			} else {
				stages := make([]json.RawMessage, len(tt.pipeline))
				for i, s := range tt.pipeline {
					stages[i] = json.RawMessage(s)
				}
				_, err = d.AggregateDocuments(ctx, a, database.AggregateRequest{Database: "app", Collection: "items", Pipeline: stages})
			}
			var safe *database.Error
			if !errors.As(err, &safe) || safe.Code != tt.code {
				t.Fatalf("error %v, want %s", err, tt.code)
			}
			want := before
			if tt.code == contracts.ConnectFailed {
				want++
			}
			if got := accepts.Load(); got != want {
				t.Fatalf("%d connections, want %d", got-before, want-before)
			}
		})
	}
}
