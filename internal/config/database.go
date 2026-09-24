package config

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"
)

const databasePath = "state/data-mate.db"

// Ciphertext is an opaque authenticated bundle. This boundary accepts no secrets.
type Ciphertext struct {
	Reference    string
	ConnectionID string
	Version      int
	Data         []byte
}

// KeysetMetadata contains only nonsecret lifecycle and usage information.
type KeysetMetadata struct {
	Account     string
	Phase       string
	Fingerprint string
	Reserved    uint64
}

func (s *Store) checkObsolete() error {
	for _, path := range []string{"config/connections.json", "config/connections.json.tmp", "state/vault.json", "state/vault.json.tmp", "state/vault-usage.json", "state/vault-usage.json.tmp"} {
		var st unix.Stat_t
		err := unix.Fstatat(int(s.dir.Fd()), path, &st, unix.AT_SYMLINK_NOFOLLOW)
		if err == nil {
			return ErrObsolete
		}
		if !errors.Is(err, unix.ENOENT) {
			return ErrOwnership
		}
	}
	return nil
}

func (s *Store) openDatabase(ctx context.Context, write, create bool) (*sql.DB, error) {
	if err := s.validRoot(); err != nil {
		return nil, err
	}
	if err := s.checkObsolete(); err != nil {
		return nil, err
	}
	for _, path := range []string{databasePath, databasePath + "-journal"} {
		f, err := s.openFile(path, unix.O_RDONLY, 0600)
		if errors.Is(err, os.ErrNotExist) {
			if path == databasePath && !create {
				return nil, ErrState
			}
			continue
		}
		if err != nil {
			return nil, ErrOwnership
		}
		f.Close()
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		var st unix.Stat_t
		e := unix.Fstatat(int(s.dir.Fd()), databasePath+suffix, &st, unix.AT_SYMLINK_NOFOLLOW)
		if !errors.Is(e, unix.ENOENT) {
			return nil, ErrState
		}
	}
	if create {
		f, err := s.openFile(databasePath, unix.O_RDWR|unix.O_CREAT, 0600)
		if err != nil {
			return nil, err
		}
		f.Close()
	}
	mode := "ro"
	if write {
		mode = "rw"
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(s.root.Path, databasePath)}
	q := u.Query()
	q.Set("mode", mode)
	q.Set("_foreign_keys", "on")
	q.Set("_busy_timeout", "0")
	q.Set("_synchronous", "EXTRA")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, ErrState
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(e error) (*sql.DB, error) { db.Close(); return nil, e }
	if err = db.PingContext(ctx); err != nil {
		if !write {
			return fail(ErrRecovery)
		}
		return fail(ErrState)
	}
	for _, pragma := range []string{"PRAGMA temp_store=MEMORY", "PRAGMA fullfsync=ON"} {
		if _, err = db.ExecContext(ctx, pragma); err != nil {
			return fail(ErrState)
		}
	}
	var journal string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "delete" {
		return fail(ErrState)
	}
	for pragma, expected := range map[string]int{"foreign_keys": 1, "synchronous": 3, "temp_store": 2, "fullfsync": 1} {
		var value int
		if err = db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&value); err != nil || value != expected {
			return fail(ErrState)
		}
	}
	return db, nil
}

func (s *Store) initializeDatabase(ctx context.Context) error {
	// Open owns lifecycle; take the remaining stable locks before SQLite recovery.
	unlock, err := s.local.state.acquire(ctx, true)
	if err != nil {
		return err
	}
	defer unlock()
	gate, err := s.lock(ctx, "state/state-gate.lock", true, false)
	if err != nil {
		return err
	}
	defer releaseFile(gate)
	state, err := s.lock(ctx, "state/state.lock", true, false)
	if err != nil {
		return err
	}
	defer releaseFile(state)
	db, err := s.openDatabase(ctx, true, !s.identity.ProfilesEstablished)
	if err != nil {
		return err
	}
	defer db.Close()
	if !s.identity.ProfilesEstablished {
		// A nonempty database can be an interrupted initialization, but never grants
		// permission to add tables to a database belonging to another installation.
		var tables int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			return ErrState
		}
		if tables > 0 {
			return validateDatabase(ctx, db, s.identity)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return ErrState
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS installation (
   singleton INTEGER PRIMARY KEY CHECK(singleton=1), schema_version INTEGER NOT NULL CHECK(schema_version=1),
   installation_id TEXT NOT NULL, root_digest TEXT NOT NULL, generation INTEGER NOT NULL CHECK(generation>=0));
   CREATE TABLE IF NOT EXISTS profiles (
   id TEXT PRIMARY KEY, alias TEXT NOT NULL UNIQUE, settings BLOB NOT NULL CHECK(length(settings)<=1048576),
   credential_ref TEXT UNIQUE, UNIQUE(id,credential_ref),
   FOREIGN KEY(credential_ref,id) REFERENCES bundles(reference,connection_id) DEFERRABLE INITIALLY DEFERRED);
   CREATE TABLE IF NOT EXISTS bundles (
   reference TEXT PRIMARY KEY, connection_id TEXT NOT NULL UNIQUE, version INTEGER NOT NULL CHECK(version=1),
   ciphertext BLOB NOT NULL CHECK(length(ciphertext)>0 AND length(ciphertext)<=4194337), UNIQUE(reference,connection_id),
   FOREIGN KEY(connection_id,reference) REFERENCES profiles(id,credential_ref) DEFERRABLE INITIALLY DEFERRED);
   CREATE TABLE IF NOT EXISTS keyset (
   singleton INTEGER PRIMARY KEY CHECK(singleton=1), account TEXT NOT NULL, phase TEXT NOT NULL CHECK(phase IN ('intent','ready')),
   fingerprint TEXT NOT NULL, reserved INTEGER NOT NULL CHECK(reserved BETWEEN 0 AND 1000000));`)
		if err != nil {
			return ErrState
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO installation VALUES(1,1,?,?,0)", s.identity.ID, s.identity.RootDigest); err != nil {
			return ErrState
		}
		if err = tx.Commit(); err != nil {
			return ErrState
		}
	}
	return validateDatabase(ctx, db, s.identity)
}

func validateDatabase(ctx context.Context, db *sql.DB, id Identity) error {
	var version int
	var installation, root string
	var generation int64
	if err := db.QueryRowContext(ctx, "SELECT schema_version,substr(installation_id,1,37),substr(root_digest,1,65),generation FROM installation WHERE singleton=1").Scan(&version, &installation, &root, &generation); err != nil || version != 1 || installation != id.ID || root != id.RootDigest || generation < 0 {
		return ErrState
	}
	return nil
}

func (l *Lease) database() (*sql.DB, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	if l.ctx == nil {
		l.ctx = context.Background()
	}
	if l.db == nil {
		if !l.write && !l.databasePermit {
			select {
			case l.store.local.databaseSlots <- struct{}{}:
				l.databasePermit = true
			case <-l.ctx.Done():
				return nil, l.ctx.Err()
			}
		}
		db, err := l.store.openDatabase(l.ctx, l.write, false)
		if err != nil {
			return nil, err
		}
		if err = validateDatabase(l.ctx, db, l.Identity()); err != nil {
			db.Close()
			return nil, err
		}
		l.db = db
	}
	return l.db, nil
}

func databaseRevision(digest Revision, generation uint64) Revision {
	return Revision(fmt.Sprintf("%s:%d", digest, generation))
}

// ProfileSnapshot reads fresh validated nonsecret rows while retaining state access.
func (l *Lease) ProfileSnapshot() (Profiles, Revision, error) {
	db, err := l.database()
	if err != nil {
		return Profiles{}, "", err
	}
	var generation uint64
	if err = db.QueryRowContext(l.ctx, "SELECT generation FROM installation WHERE singleton=1").Scan(&generation); err != nil {
		return Profiles{}, "", ErrState
	}
	rows, err := db.QueryContext(l.ctx, "SELECT id,alias,credential_ref,substr(settings,1,1048577) FROM profiles ORDER BY id LIMIT 129")
	if err != nil {
		return Profiles{}, "", ErrState
	}
	defer rows.Close()
	p := Profiles{Version: 1, Connections: []Profile{}}
	size := 0
	for rows.Next() {
		var id, alias string
		var ref sql.NullString
		var raw []byte
		if rows.Scan(&id, &alias, &ref, &raw) != nil {
			return Profiles{}, "", ErrState
		}
		size += len(raw)
		if size > MaxProfileBytes || len(p.Connections) >= 128 {
			return Profiles{}, "", ErrState
		}
		var profile Profile
		if DecodeStrict(raw, MaxProfileBytes, &profile) != nil || profile.ID != id || profile.Alias != alias || profile.CredentialRef != ref.String {
			return Profiles{}, "", ErrState
		}
		p.Connections = append(p.Connections, profile)
	}
	if rows.Err() != nil {
		return Profiles{}, "", ErrState
	}
	raw, _ := json.Marshal(p)
	p, digest, err := DecodeProfiles(bytes.NewReader(raw))
	return p, databaseRevision(digest, generation), err
}

// Bundle returns bounded opaque ciphertext; absence remains repairable by db edit.
func (l *Lease) Bundle(ref string) (Ciphertext, error) {
	db, err := l.database()
	if err != nil {
		return Ciphertext{}, err
	}
	b := Ciphertext{Reference: ref}
	var size int
	err = db.QueryRowContext(l.ctx, "SELECT substr(connection_id,1,37),version,length(ciphertext),substr(ciphertext,1,4194337) FROM bundles WHERE reference=?", ref).Scan(&b.ConnectionID, &b.Version, &size, &b.Data)
	if err == nil && (size < 1 || size > 4194337) {
		return b, ErrState
	}
	if errors.Is(err, sql.ErrNoRows) {
		return b, os.ErrNotExist
	}
	if err != nil {
		return b, ErrState
	}
	return b, nil
}

// Keyset returns metadata without accessing OS storage.
func (l *Lease) Keyset() (KeysetMetadata, error) {
	db, err := l.database()
	if err != nil {
		return KeysetMetadata{}, err
	}
	var k KeysetMetadata
	err = db.QueryRowContext(l.ctx, "SELECT substr(account,1,65),substr(phase,1,7),substr(fingerprint,1,65),reserved FROM keyset WHERE singleton=1").Scan(&k.Account, &k.Phase, &k.Fingerprint, &k.Reserved)
	if errors.Is(err, sql.ErrNoRows) {
		var count int
		if db.QueryRowContext(l.ctx, "SELECT count(*) FROM bundles").Scan(&count) != nil || count != 0 {
			return k, ErrState
		}
		return k, nil
	}
	if err != nil || k.Account != l.Identity().KeyAccount || (k.Phase != "intent" && k.Phase != "ready") || len(k.Fingerprint) != 64 || k.Reserved > 1000000 {
		return k, ErrState
	}
	decoded, e := hex.DecodeString(k.Fingerprint)
	if e != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != k.Fingerprint {
		return k, ErrState
	}
	var count, total int
	if db.QueryRowContext(l.ctx, "SELECT count(*),COALESCE(sum(length(ciphertext)),0) FROM bundles").Scan(&count, &total) != nil || count > 128 || total > 8<<20 || (count > 0 && (k.Phase != "ready" || k.Reserved < uint64(count))) {
		return k, ErrState
	}
	return k, nil
}

// SaveKeyset commits only nonsecret lifecycle metadata under exclusive state access.
func (l *Lease) SaveKeyset(k KeysetMetadata) error {
	if !l.write || k.Account != l.Identity().KeyAccount {
		return ErrOwnership
	}
	db, err := l.database()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(l.ctx, "INSERT INTO keyset VALUES(1,?,?,?,?) ON CONFLICT(singleton) DO UPDATE SET account=excluded.account,phase=excluded.phase,fingerprint=excluded.fingerprint,reserved=excluded.reserved", k.Account, k.Phase, k.Fingerprint, k.Reserved)
	if err != nil {
		return ErrState
	}
	return nil
}

// SaveProfiles publishes a nonsecret mutation with the same atomic contract.
func (l *Lease) SaveProfiles(p Profiles, expected Revision) (Revision, error) {
	return l.Publish(p, expected, nil)
}

// Publish commits profiles, ciphertext replacements, removals and generation together.
func (l *Lease) Publish(p Profiles, expected Revision, replacements []Ciphertext) (Revision, error) {
	if !l.write {
		return "", ErrOwnership
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", ErrState
	}
	p, digest, err := DecodeProfiles(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	_, rev, err := l.ProfileSnapshot()
	if err != nil {
		return "", err
	}
	if rev != expected {
		return "", ErrRevision
	}
	db, err := l.database()
	if err != nil {
		return "", err
	}
	tx, err := db.BeginTx(l.ctx, nil)
	if err != nil {
		return "", ErrState
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(l.ctx, "DELETE FROM profiles"); err != nil {
		return "", ErrState
	}
	for _, b := range replacements {
		if _, err = tx.ExecContext(l.ctx, "DELETE FROM bundles WHERE connection_id=?", b.ConnectionID); err != nil {
			return "", ErrState
		}
		if _, err = tx.ExecContext(l.ctx, "INSERT INTO bundles VALUES(?,?,?,?)", b.Reference, b.ConnectionID, b.Version, b.Data); err != nil {
			return "", ErrState
		}
	}
	for _, profile := range p.Connections {
		data, _ := json.Marshal(profile)
		var ref any
		if profile.CredentialRef != "" {
			ref = profile.CredentialRef
		}
		if _, err = tx.ExecContext(l.ctx, "INSERT INTO profiles VALUES(?,?,?,?)", profile.ID, profile.Alias, data, ref); err != nil {
			return "", ErrState
		}
	}
	if _, err = tx.ExecContext(l.ctx, "DELETE FROM bundles WHERE reference NOT IN (SELECT credential_ref FROM profiles WHERE credential_ref IS NOT NULL)"); err != nil {
		return "", ErrState
	}
	var total int
	if tx.QueryRowContext(l.ctx, "SELECT COALESCE(sum(length(ciphertext)),0) FROM bundles").Scan(&total) != nil || total > 8<<20 {
		return "", ErrState
	}
	var generation uint64
	if err = tx.QueryRowContext(l.ctx, "UPDATE installation SET generation=generation+1 WHERE singleton=1 RETURNING generation").Scan(&generation); err != nil {
		return "", ErrState
	}
	if err = l.store.point("before-commit", databasePath); err != nil {
		return "", err
	}
	if err = l.ctx.Err(); err != nil {
		return "", err
	}
	if err = l.check(); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", ErrCommitUnknown
	}
	if err = l.store.point("after-commit", databasePath); err != nil {
		return "", errors.Join(ErrCommitUnknown, err)
	}
	return databaseRevision(digest, generation), nil
}
