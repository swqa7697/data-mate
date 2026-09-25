package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/service"
)

func scopeSelected(cmd *cobra.Command) bool {
	return flag(cmd, "all") || flag(cmd, "none") || changed(cmd, "scope-json", "schema", "table")
}
func selectScope(cmd *cobra.Command, root config.Root, revision config.Revision, p config.Profile, f *form, factory managementFactory) (config.Scope, error) {
	client, err := factory(cmd.Context(), root)
	if err != nil {
		return config.Scope{}, serviceError(err)
	}
	defer client.Close()
	fetch := func(req database.ScopeRequest) (database.ScopePage, error) {
		reply, err := client.Request(cmd.Context(), service.ManagementRequest{Operation: "browse", Interactive: true, Expected: revision, ProfileID: p.ID, Alias: p.Alias, Scope: &req})
		if err != nil {
			return database.ScopePage{}, storageError(err, false)
		}
		if reply.Page == nil {
			return database.ScopePage{}, failure("catalog response unavailable")
		}
		return *reply.Page, nil
	}
	return scopePicker(cmd, f, p.Scope, fetch)
}

// The picker retains a single catalog page and the user's bounded selection.
// Unseen/missing existing names survive browsing; all/none explicitly replaces
// them. Database leases cover fetch and cleanup, never human think time.
func scopePicker(cmd *cobra.Command, f *form, initial config.Scope, fetch func(database.ScopeRequest) (database.ScopePage, error)) (config.Scope, error) {
	selected := config.Scope{Mode: initial.Mode, Schemas: slices.Clone(initial.Schemas), Tables: slices.Clone(initial.Tables)}
	req := database.ScopeRequest{}
	page, err := fetch(req)
	if err != nil {
		return config.Scope{}, err
	}
	pos := 0
	note := ""
	render := func() error {
		if _, err := fmt.Fprint(f.terminal, "\x1b[2J\x1b[HScope selection: arrows move; Right expands; Left returns to schemas.\nSpace toggles; / searches this level; n next page; b first page.\na = all; 0 = none; Enter previews; q or Ctrl-C cancels.\nWhole schemas include current and future tables. Partition roots include their partitions.\nSelections use exact names; catalog visibility does not authorize unsupported queries.\n"); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(f.terminal, "Level: %q  Search: %q  Mode: %s  (%d schemas, %d tables)\n", req.Schema, req.Search, selected.Mode, len(selected.Schemas), len(selected.Tables)); err != nil {
			return err
		}
		if note != "" {
			if _, err := fmt.Fprintln(f.terminal, note); err != nil {
				return err
			}
		}
		count := len(page.Schemas) + len(page.Tables)
		if count == 0 {
			_, err := fmt.Fprintln(f.terminal, "No matching accessible objects.")
			return err
		}
		start := max(0, pos-5)
		end := min(count, start+12)
		for i := start; i < end; i++ {
			cursor := " "
			if i == pos {
				cursor = ">"
			}
			var name string
			var chosen bool
			if req.Schema == "" {
				name = fmt.Sprintf("%q — all current and future tables", page.Schemas[i])
				chosen = selected.Mode == "all" || slices.Contains(selected.Schemas, page.Schemas[i])
			} else {
				t := page.Tables[i]
				name = fmt.Sprintf("%q.%q", t.Schema, t.Name)
				chosen = selected.ContainsName(t.Schema, t.Name)
				if t.Kind == "partitioned_table" {
					name += " (includes partitions)"
				}
			}
			mark := " "
			if chosen {
				mark = "x"
			}
			if _, err := fmt.Fprintf(f.terminal, "%s [%s] %s\n", cursor, mark, name); err != nil {
				return err
			}
		}
		if page.Next != "" {
			_, err := fmt.Fprintln(f.terminal, "More objects: n")
			return err
		}
		return nil
	}
	reader := commandInput(cmd)
	key := func() (byte, error) {
		var b [1]byte
		_, err := io.ReadFull(reader, b[:])
		if err != nil {
			return 0, context.Canceled
		}
		return b[0], nil
	}
	for {
		if err := render(); err != nil {
			return config.Scope{}, failure("cannot render scope selection")
		}
		k, err := key()
		if err != nil {
			return config.Scope{}, err
		}
		note = ""
		count := len(page.Schemas) + len(page.Tables)
		refresh := false
		if k == 27 {
			a, e := key()
			if e != nil {
				return config.Scope{}, e
			}
			if a != '[' {
				continue
			}
			k, e = key()
			if e != nil {
				return config.Scope{}, e
			}
			switch k {
			case 'A':
				pos = max(0, pos-1)
			case 'B':
				pos = min(max(0, count-1), pos+1)
			case 'C':
				if req.Schema == "" && count > 0 {
					req = database.ScopeRequest{Schema: page.Schemas[pos]}
					refresh = true
				}
			case 'D':
				req = database.ScopeRequest{}
				refresh = true
			}
		} else {
			switch k {
			case 3, 4, 'q':
				return config.Scope{}, context.Canceled
			case '\r', '\n':
				return selected, nil
			case 'a':
				selected = config.Scope{Mode: "all"}
			case '0':
				selected = config.Scope{Mode: "selected"}
			case 'n':
				if page.Next != "" {
					req.After = page.Next
					refresh = true
				}
			case 'b':
				req.After = ""
				refresh = true
			case '/':
				search, e := f.ask("Search (blank clears)", "", false)
				if e != nil {
					return config.Scope{}, e
				}
				if len(search) > 256 || strings.ContainsRune(search, 0) {
					return config.Scope{}, invalid("search must be at most 256 bytes")
				}
				req.Search = search
				req.After = ""
				refresh = true
			case ' ':
				if count == 0 {
					continue
				}
				// Broad selections cannot express exclusions. Require an explicit narrow
				// starting point instead of silently dropping unseen selections.
				if selected.Mode == "all" {
					note = "Choose 0 (none) before selecting individual schemas or tables."
					continue
				}
				if req.Schema == "" {
					name := page.Schemas[pos]
					if slices.Contains(selected.Schemas, name) {
						selected.Schemas = slices.DeleteFunc(selected.Schemas, func(v string) bool { return v == name })
					} else {
						selected.Schemas = append(selected.Schemas, name)
					}
				} else {
					t := page.Tables[pos]
					name := config.Table{Schema: t.Schema, Name: t.Name}
					if slices.Contains(selected.Schemas, t.Schema) {
						note = "Return Left and deselect the whole schema before choosing fixed tables."
						continue
					}
					if slices.Contains(selected.Tables, name) {
						selected.Tables = slices.DeleteFunc(selected.Tables, func(v config.Table) bool { return v == name })
					} else {
						selected.Tables = append(selected.Tables, name)
					}
				}
				raw, _ := json.Marshal(selected)
				if len(raw) > config.MaxProfileBytes/2 || len(selected.Schemas) > 4096 || len(selected.Tables) > 4096 {
					return config.Scope{}, invalid("scope selection exceeds limit; use whole schemas")
				}
			}
		}
		if refresh {
			page, err = fetch(req)
			if err != nil {
				return config.Scope{}, err
			}
			pos = 0
		}
	}
}
