package mongodb

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// A synthetic peer can send topologies, hostile SCRAM messages, privilege
// documents and malformed frames that the supported Docker images cannot
// produce. Each case checks the diagnostic stage, the public code and that
// server text, which carries the secret, is never surfaced.
func TestMongoDBConnectionBoundary(t *testing.T) {
	credentials := scramCredentials(t)
	secondary := helloReply(25, kv("setName", "rs0"), kv("isWritablePrimary", false), kv("secondary", true), kv("hidden", true))
	arbiter := helloReply(25, kv("setName", "rs0"), kv("arbiterOnly", true))
	withPrivilege := func(extra bson.D) bson.A { return append(readOnlyPrivileges(), extra) }
	raw := func(build func(id int32) []byte) func(bson.Raw, int32) []byte {
		return func(_ bson.Raw, id int32) []byte { return build(id) }
	}
	deep := bson.Raw{5, 0, 0, 0, 0}
	for range 300 {
		doc := binary.LittleEndian.AppendUint32(nil, uint32(4+3+len(deep)+1))
		doc = append(append(append(doc, 0x03, 'a', 0), deep...), 0)
		deep = doc
	}
	for _, tt := range []struct {
		name, stage string
		code        contracts.Code
		member      bool
		script      func(sp *scramPeer) []exchange
	}{
		{"read-only standalone with a checksummed hello", "read_only", "", false, func(sp *scramPeer) []exchange {
			return append([]exchange{hello(raw(func(id int32) []byte { return opMsgFrame(id, 0, marshal(standalone), true) }))}, ready(sp)[1:]...)
		}},
		{"hidden secondary reads with primaryPreferred", "read_only", "", true, func(sp *scramPeer) []exchange {
			return append([]exchange{hello(send(secondary))}, ready(sp)[1:]...)
		}},
		{"mongos uses its router handshake", "read_only", "", false, func(sp *scramPeer) []exchange {
			router := helloReply(25, kv("isWritablePrimary", true), kv("msg", "isdbgrid"))
			return append([]exchange{hello(send(router))}, ready(sp)[1:]...)
		}},
		{"arbiter is rejected before credentials", "version", contracts.QueryUnsupported, false, func(*scramPeer) []exchange {
			return []exchange{hello(send(arbiter))}
		}},
		{"secondary without a set name", "version", contracts.QueryUnsupported, false, func(*scramPeer) []exchange {
			return []exchange{hello(send(helloReply(25, kv("isWritablePrimary", false), kv("secondary", true))))}
		}},
		{"member reports two roles", "version", contracts.QueryUnsupported, false, func(*scramPeer) []exchange {
			return []exchange{hello(send(helloReply(25, kv("setName", "rs0"), kv("isWritablePrimary", true), kv("secondary", true))))}
		}},
		{"wire version below 7.0", "version", contracts.QueryUnsupported, false, func(*scramPeer) []exchange {
			return []exchange{hello(send(helloReply(17, kv("isWritablePrimary", true))))}
		}},
		{"authentication failure redacts server text", "authentication", contracts.ConnectFailed, false, func(sp *scramPeer) []exchange {
			return []exchange{hello(send(standalone)), sp.start(nil), {command: "saslContinue", check: checkSASL, reply: send(failure(18))}}
		}},
		{"server signature does not verify", "authentication", contracts.ConnectFailed, false, func(sp *scramPeer) []exchange {
			forged := func(string) string { return "v=" + base64.StdEncoding.EncodeToString(make([]byte, 32)) }
			return []exchange{hello(send(standalone)), sp.start(nil), sp.finish(forged)}
		}},
		{"iteration count above the cap stops before key derivation", "authentication", contracts.ConnectFailed, false, func(sp *scramPeer) []exchange {
			costly := func(s string) string { return strings.Replace(s, "i=4096", "i=2000000", 1) }
			return []exchange{hello(send(standalone)), sp.start(costly)}
		}},
		{"authenticated identity differs from the profile", "authentication", contracts.ConnectFailed, false, func(sp *scramPeer) []exchange {
			return []exchange{hello(send(standalone)), sp.start(nil), sp.finish(nil), identity(fakeUser, "admin")}
		}},
		{"write action", "read_only", contracts.ReadOnlyViolation, false, func(sp *scramPeer) []exchange {
			return append(handshake(sp), audit(withPrivilege(privilege(bson.D{kv("db", "app"), kv("collection", "items")}, "find", "insert"))))
		}},
		{"read of a credential collection in any database", "read_only", contracts.ReadOnlyViolation, false, func(sp *scramPeer) []exchange {
			return append(handshake(sp), audit(withPrivilege(privilege(bson.D{kv("db", ""), kv("collection", "system.users")}, "find"))))
		}},
		{"data read on every resource", "read_only", contracts.ReadOnlyViolation, false, func(sp *scramPeer) []exchange {
			return append(handshake(sp), audit(withPrivilege(privilege(bson.D{kv("anyResource", true)}, "find"))))
		}},
		{"unknown resource shape", "read_only", contracts.ReadOnlyViolation, false, func(sp *scramPeer) []exchange {
			return append(handshake(sp), audit(withPrivilege(privilege(bson.D{kv("db", "app"), kv("collection", "items"), kv("tenant", "x")}, "find"))))
		}},
		{"server without authorization answers the probe", "read_only", contracts.ReadOnlyViolation, false, func(sp *scramPeer) []exchange {
			return append(handshake(sp), audit(readOnlyPrivileges()), probe(ok(kv("users", bson.A{}))))
		}},
		{"reply length over the control limit", "dial", contracts.ResourceLimit, false, func(*scramPeer) []exchange {
			// Only a header arrives: its length is refused before anything else is read or allocated.
			return []exchange{hello(raw(func(id int32) []byte {
				header := opMsgFrame(id, 0, nil, false)[:16]
				binary.LittleEndian.PutUint32(header, 9_000_000)
				return header
			}))}
		}},
		{"compressed reply", "dial", contracts.ConnectFailed, false, func(*scramPeer) []exchange {
			return []exchange{hello(raw(func(id int32) []byte { return frame(opCompressed, id, 0, marshal(standalone), false) }))}
		}},
		{"exhaust reply", "dial", contracts.ConnectFailed, false, func(*scramPeer) []exchange {
			return []exchange{hello(raw(func(id int32) []byte { return opMsgFrame(id, flagMoreToCome, marshal(standalone), false) }))}
		}},
		{"checksum mismatch", "dial", contracts.ConnectFailed, false, func(*scramPeer) []exchange {
			return []exchange{hello(raw(func(id int32) []byte {
				b := opMsgFrame(id, 0, marshal(standalone), true)
				b[len(b)-1] ^= 0xff
				return b
			}))}
		}},
		{"reply nesting over the depth limit", "dial", contracts.ResourceLimit, false, func(*scramPeer) []exchange {
			return []exchange{hello(send(append(standalone, kv("deep", deep))))}
		}},
		{"cancellation during authentication", "authentication", contracts.Cancelled, false, func(*scramPeer) []exchange {
			return []exchange{hello(send(standalone)), {command: "saslStart", check: checkSASL}}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			peer := newPeer(t, cancel, tt.member, tt.script(newScramPeer(credentials)))
			out, err := New(fakeRegistry(t)).Test(ctx, database.NewAccess(fakeProfile(peer.port(), config.DefaultLimits()), fakeSecret))
			if out.Stage != tt.stage {
				t.Fatalf("stage %q, error %v", out.Stage, err)
			}
			var safe *database.Error
			if tt.code == "" {
				if err != nil || out.ServerVersion != 70048 {
					t.Fatalf("unexpected readiness %+v, error %v", out, err)
				}
			} else if !errors.As(err, &safe) || safe.Code != tt.code || strings.Contains(err.Error(), fakeSecret) {
				t.Fatalf("incorrect safe error %v", err)
			}
			peer.wait(t)
		})
	}

	t.Run("two checked-out connections each earn approval", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		empty := ok(kv("cursor", bson.D{kv("id", int64(0)), kv("ns", "app.items"), kv("firstBatch", bson.A{})}))
		peer := newPeer(t, cancel, false,
			ready(newScramPeer(credentials)),
			ready(newScramPeer(credentials), respond("find", empty)))
		a, rev, err := normalizeAccess(database.NewAccess(fakeProfile(peer.port(), config.DefaultLimits()), fakeSecret))
		if err != nil {
			t.Fatal(err)
		}
		r := fakeRegistry(t)
		d := New(r)
		// Hold the first lease while find opens a second physical session.
		err = d.runNormalized(ctx, a, rev, func(ctx context.Context, _ *session) error {
			_, err := d.FindDocuments(ctx, a, database.FindRequest{Database: "app", Collection: "items"})
			return err
		})
		r.Close()
		peer.wait(t)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Approval expires as soon as the final physical connection disconnects,
	// even when no operation has checked the connection out since its last use.
	t.Run("idle session observes peer disconnect", func(t *testing.T) {
		conn, other := net.Pipe()
		s := newSession(conn, conn)
		defer s.Close()
		other.Close()
		select {
		case <-s.Closed():
			if s.Reusable() {
				t.Fatal("disconnected MongoDB session is still reusable")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("idle MongoDB session did not report peer disconnect")
		}
	})
}
