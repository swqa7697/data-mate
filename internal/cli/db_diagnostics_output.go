package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rivo/uniseg"
	"github.com/swqa7697/data-mate/internal/service"
	"golang.org/x/term"
)

var (
	diagnosticSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	colorSequence     = regexp.MustCompile("\x1b\\[[0-9;]*m")
)

// diagnosticBoard collects streamed results by alias for display in list order.
type diagnosticBoard struct {
	mu      sync.Mutex
	aliases []string
	index   map[string]int
	results []*diagnosticResult
	changed chan struct{}
}

func newDiagnosticBoard(aliases []string) *diagnosticBoard {
	b := &diagnosticBoard{aliases: aliases, index: make(map[string]int, len(aliases)), results: make([]*diagnosticResult, len(aliases)), changed: make(chan struct{}, 1)}
	for i, alias := range aliases {
		b.index[alias] = i
	}
	return b
}

// report accepts one result per requested alias. It never blocks, so slow
// output cannot stall the management exchange.
func (b *diagnosticBoard) report(r diagnosticResult) error {
	b.mu.Lock()
	i, ok := b.index[r.Alias]
	if !ok || b.results[i] != nil {
		b.mu.Unlock()
		return service.ErrState
	}
	b.results[i] = &r
	b.mu.Unlock()
	select {
	case b.changed <- struct{}{}:
	default:
	}
	return nil
}

// snapshot returns results in list order; nil entries are still pending.
func (b *diagnosticBoard) snapshot() []*diagnosticResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.results)
}

// complete returns every result in list order, or false while any is pending.
func (b *diagnosticBoard) complete() ([]diagnosticResult, bool) {
	results := make([]diagnosticResult, 0, len(b.aliases))
	for _, r := range b.snapshot() {
		if r == nil {
			return nil, false
		}
		results = append(results, *r)
	}
	return results, true
}

// diagnosticLines renders one summary line, followed only by failed checks.
// Pending rows show the indicator in place of the status label.
func diagnosticLines(alias string, r *diagnosticResult, indicator string, color bool) []string {
	if r == nil {
		return []string{alias + "  " + indicator}
	}
	status, tint := "PASS", "\x1b[32m"
	if !r.OK {
		status, tint = "FAIL", "\x1b[31m"
	}
	if color {
		status = tint + status + "\x1b[0m"
	}
	lines := []string{alias + "  " + status}
	for _, s := range r.Stages {
		if !s.OK {
			lines = append(lines, fmt.Sprintf("  %s: %s: %s", s.Stage, s.Error.Code, s.Error.Message))
		}
	}
	return lines
}

type diagnosticRow struct {
	lines []string
	done  bool
}

// diagnosticView is one display update. Committed lines are written once and
// scroll normally; the window is redrawn in place and occupies height rows.
type diagnosticView struct {
	commit, window []string
	height         int
	committed      int
}

// diagnosticFrame commits completed rows up to the first pending row. The
// window starts there and keeps whole rows within height-1 terminal rows, so
// relative cursor movement never leaves the visible screen.
func diagnosticFrame(rows []diagnosticRow, committed, width, height int) diagnosticView {
	v := diagnosticView{committed: committed}
	for v.committed < len(rows) && rows[v.committed].done {
		v.commit = append(v.commit, rows[v.committed].lines...)
		v.committed++
	}
	room := height - 1
	for _, row := range rows[v.committed:] {
		used := 0
		for _, line := range row.lines {
			used += max(1, (uniseg.StringWidth(colorSequence.ReplaceAllString(line, ""))+width-1)/width)
		}
		if used > room {
			break
		}
		v.window = append(v.window, row.lines...)
		v.height += used
		room -= used
	}
	return v
}

// diagnosticDisplay writes board rows as results arrive: a live window with
// pending indicators on terminals, or completed rows in order otherwise.
type diagnosticDisplay struct {
	out   io.Writer
	board *diagnosticBoard
	color bool
	live  bool
	size  func() (int, int)

	mu        sync.Mutex
	paused    bool
	stopped   bool
	committed int
	drawn     int
	phase     int
	err       error
	quit      chan struct{}
	done      chan struct{}
}

// startDiagnosticDisplay draws pending rows immediately on a terminal and
// redraws them until stop.
func startDiagnosticDisplay(w io.Writer, board *diagnosticBoard) *diagnosticDisplay {
	d := &diagnosticDisplay{out: w, board: board, quit: make(chan struct{}), done: make(chan struct{})}
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, noColor := os.LookupEnv("NO_COLOR")
		terminal := os.Getenv("TERM")
		d.color = !noColor
		d.live = terminal != "" && terminal != "dumb"
		d.size = func() (int, int) {
			width, height, err := term.GetSize(int(f.Fd()))
			if err != nil || width <= 0 || height <= 0 {
				return 80, 24
			}
			return width, height
		}
	}
	if d.live {
		d.draw(false)
	}
	go d.run()
	return d
}

func (d *diagnosticDisplay) run() {
	defer close(d.done)
	var tick <-chan time.Time
	if d.live {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-d.quit:
			return
		case <-d.board.changed:
		case <-tick:
		}
		d.draw(false)
	}
}

// draw writes one update. The final update keeps every completed row in order
// and omits pending rows.
func (d *diagnosticDisplay) draw(final bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.paused || d.stopped || d.err != nil {
		return
	}
	d.phase++
	results := d.board.snapshot()
	rows := make([]diagnosticRow, len(results))
	for i, r := range results {
		rows[i] = diagnosticRow{diagnosticLines(d.board.aliases[i], r, diagnosticSpinner[d.phase%len(diagnosticSpinner)], d.color), r != nil}
	}
	var v diagnosticView
	if final {
		v.committed = len(rows)
		for _, row := range rows[d.committed:] {
			if row.done {
				v.commit = append(v.commit, row.lines...)
			}
		}
	} else {
		// Without a live window, plain output only commits completed rows.
		width, height := 1, 1
		if d.live {
			width, height = d.size()
		}
		v = diagnosticFrame(rows, d.committed, width, height)
	}
	var b strings.Builder
	lines := slices.Concat(v.commit, v.window)
	if d.live && (d.drawn > 0 || len(lines) > 0) {
		b.WriteString(d.erase())
	}
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	d.committed, d.drawn = v.committed, v.height
	if b.Len() > 0 {
		_, d.err = io.WriteString(d.out, b.String())
	}
}

// erase returns to column zero of the live window and clears it. Echoed
// input such as ^C may have moved the cursor right.
func (d *diagnosticDisplay) erase() string {
	if d.drawn == 0 {
		return "\r\x1b[J"
	}
	return fmt.Sprintf("\r\x1b[%dA\x1b[J", d.drawn)
}

// pause clears the live window before a terminal prompt; nothing is drawn
// while the prompt owns the terminal, which may be in raw mode.
func (d *diagnosticDisplay) pause() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.paused || d.stopped || d.err != nil || !d.live {
		return
	}
	d.paused = true
	if d.drawn > 0 {
		_, d.err = io.WriteString(d.out, d.erase())
		d.drawn = 0
	}
}

func (d *diagnosticDisplay) resume() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused = false
}

// stop writes the final rows and reports the first output error. A prompt
// abandoned by cancellation may still own the terminal, so a paused display
// writes nothing more.
func (d *diagnosticDisplay) stop() error {
	close(d.quit)
	<-d.done
	d.draw(true)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	return d.err
}
