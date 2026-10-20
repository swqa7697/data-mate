package postgres

import (
	"context"
	"encoding/binary"
	"io"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
)

// Observe only message headers so diagnostics distinguish authentication from
// dialing without retaining or inspecting secret-bearing protocol bodies.
type wireState struct{ authStarted atomic.Bool }

func (s *wireState) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (s *wireState) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type frameReader struct {
	io.Reader
	state     *wireState
	header    [5]byte
	used      int
	remaining uint32
}

func (r *frameReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	b := p[:n]
	for len(b) > 0 {
		if r.remaining > 0 {
			take := min(len(b), int(r.remaining))
			r.remaining -= uint32(take)
			b = b[take:]
			continue
		}
		take := min(len(b), 5-r.used)
		copy(r.header[r.used:], b[:take])
		r.used += take
		b = b[take:]
		if r.used == 5 {
			if r.header[0] == 'R' {
				r.state.authStarted.Store(true)
			}
			size := binary.BigEndian.Uint32(r.header[1:])
			r.used = 0
			if size < 4 {
				return n, err
			}
			r.remaining = size - 4
		}
	}
	return n, err
}
func trackWire(c *pgx.ConnConfig) *wireState {
	s := &wireState{}
	c.Tracer = s
	c.BuildFrontend = func(r io.Reader, w io.Writer) *pgproto3.Frontend {
		f := pgproto3.NewFrontend(&frameReader{Reader: r, state: s}, w)
		f.SetMaxBodyLen(0)
		return f
	}
	return s
}
