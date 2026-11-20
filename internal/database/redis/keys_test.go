package redis

import (
	"errors"
	"testing"

	"github.com/swqa7697/data-mate/internal/contracts"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/database/pool"
)

// Live SCAN does not offer a way to schedule rehashing between two reads of
// exactly the same bucket. Reordering a partially consumed batch must fail
// before its offset can skip data that was never delivered.
func TestChangedScanReplay(t *testing.T) {
	r, err := pool.New(pool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	d := New(r)
	a := database.Access{}
	pos, err := d.position(a, "", "list_keys", "", "")
	if err != nil {
		t.Fatal(err)
	}
	original := []any{"9", []any{"a", "b", "c"}}
	part, more, err := d.scanWindow(original, &pos, 1, 1)
	if err != nil || !more || len(part) != 1 || part[0] != "a" {
		t.Fatalf("first scan page: %v %t %v", part, more, err)
	}
	token := d.continuation(pos)
	resume, err := d.position(a, "", "list_keys", "", *token)
	if err != nil {
		t.Fatal(err)
	}
	old := resume
	corrupt := []any{"9", []any{"c", "a", "b"}}
	_, _, err = d.scanWindow(corrupt, &resume, 2, 1)
	var safe *database.Error
	if !errors.As(err, &safe) || safe.Code != contracts.StaleCursor {
		t.Fatalf("changed scan replay: %v", err)
	}
	part, more, err = d.scanWindow(original, &old, 2, 1)
	if err != nil || !more || len(part) != 2 || part[0] != "b" || part[1] != "c" || old.Key != 9 {
		t.Fatalf("stable scan continuation: %v %t %v", part, more, err)
	}
}
