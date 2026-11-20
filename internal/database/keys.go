package database

import "context"

// KeyOperations is the key-oriented capability of the Redis driver.
type KeyOperations interface {
	ListKeys(context.Context, Access, KeyPageRequest) (KeyPage, error)
	DescribeKey(context.Context, Access, KeyRequest) (KeyDescription, error)
	ReadKey(context.Context, Access, KeyReadRequest) (KeyReadPage, error)
}

// KeyBytes carries arbitrary Redis bytes losslessly as UTF-8 or standard base64.
type KeyBytes struct {
	Value    string `json:"value"`
	Encoding string `json:"encoding"`
}

// KeyInfo identifies an authorized key without returning its contents.
type KeyInfo struct {
	Key  KeyBytes `json:"key"`
	Type string   `json:"type"`
}

// KeyPageRequest selects a page of readable keys, optionally matching a Redis glob.
type KeyPageRequest struct {
	Pattern  string
	Cursor   string
	PageSize int
}

// KeyRequest identifies one exact, binary-safe key.
type KeyRequest struct {
	Key KeyBytes
}

// KeyReadRequest selects a page of string bytes or collection entries.
type KeyReadRequest struct {
	Key      KeyBytes
	Cursor   string
	PageSize int
}

// KeyPage includes a continuation even for empty, nonterminal scan batches.
type KeyPage struct {
	Connection string    `json:"connection"`
	Driver     string    `json:"driver"`
	Keys       []KeyInfo `json:"keys"`
	NextCursor *string   `json:"next_cursor"`
}

// KeyDescription reports native length and TTL. A missing key has type none and no length.
type KeyDescription struct {
	Connection string   `json:"connection"`
	Driver     string   `json:"driver"`
	Key        KeyBytes `json:"key"`
	Type       string   `json:"type"`
	TTLMS      int64    `json:"ttl_ms"`
	Length     *int64   `json:"length"`
}

// KeyReadPage contains complete, type-specific entries. A string page is one byte range.
type KeyReadPage struct {
	Connection string   `json:"connection"`
	Driver     string   `json:"driver"`
	Key        KeyBytes `json:"key"`
	Type       string   `json:"type"`
	Items      []any    `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}
