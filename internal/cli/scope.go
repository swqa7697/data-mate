package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/database"
	"github.com/swqa7697/data-mate/internal/service"
)

func scopeSelected(cmd *cobra.Command) bool {
	return flag(cmd, "all") || flag(cmd, "none") || changed(cmd, "scope-json", "schema", "exclude-schema")
}
func selectScope(cmd *cobra.Command, root config.Root, revision config.Revision, p config.Profile, f *form, factory managementFactory) (config.Scope, error) {
	client, err := factory(cmd.Context(), root)
	if err != nil {
		return config.Scope{}, serviceError(err)
	}
	defer client.Close()
	fetch := func(ctx context.Context, req database.ScopeRequest) (database.ScopePage, error) {
		reply, err := client.Request(ctx, service.ManagementRequest{Operation: "browse", Interactive: true, Expected: revision, ProfileID: p.ID, Alias: p.Alias, Scope: &req})
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

func scopeWithinLimit(s config.Scope) bool {
	raw, err := json.Marshal(s)
	return err == nil && len(s.Schemas) <= 4096 && len(raw) <= config.MaxProfileBytes/2
}

// bulkScope gathers a bounded universe without holding database leases between
// requests. Missing saved names participate so switching modes preserves their
// explicit allowed/blocked state. No partially fetched result is ever applied.
func bulkScope(ctx context.Context, selected config.Scope, retained []string, switchMode bool, fetch func(context.Context, database.ScopeRequest) (database.ScopePage, error)) (config.Scope, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	names := slices.Clone(retained)
	for _, name := range selected.Schemas {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	req := database.ScopeRequest{}
	for {
		if err := ctx.Err(); err != nil {
			return config.Scope{}, err
		}
		page, err := fetch(ctx, req)
		if err != nil {
			return config.Scope{}, err
		}
		for _, name := range page.Schemas {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		if !scopeWithinLimit(config.Scope{Mode: selected.Mode, Schemas: names}) {
			return config.Scope{}, invalid("too many schemas for a bulk action; toggle individual schemas")
		}
		if page.Next == "" {
			break
		}
		if page.Next <= req.After {
			return config.Scope{}, failure("catalog pagination did not advance")
		}
		req.After = page.Next
	}
	if err := ctx.Err(); err != nil {
		return config.Scope{}, err
	}
	slices.Sort(names)
	result := config.Scope{Mode: selected.Mode}
	if switchMode {
		if result.Mode == "blacklist" {
			result.Mode = "whitelist"
		} else {
			result.Mode = "blacklist"
		}
		for _, name := range names {
			if !slices.Contains(selected.Schemas, name) {
				result.Schemas = append(result.Schemas, name)
			}
		}
	} else {
		all := true
		for _, name := range names {
			all = all && selected.ContainsSchema(name)
		}
		// Toggle the whole universe; retain the chosen future-schema policy.
		if (result.Mode == "whitelist" && !all) || (result.Mode == "blacklist" && all) {
			result.Schemas = names
		}
	}
	return result, nil
}

// Ordinary browsing retains one page. All changes remain local until the review
// is confirmed and published under the existing revision/ownership contracts.
func scopePicker(cmd *cobra.Command, f *form, initial config.Scope, fetch func(context.Context, database.ScopeRequest) (database.ScopePage, error)) (config.Scope, error) {
	selected := config.Scope{Mode: initial.Mode, Schemas: slices.Clone(initial.Schemas)}
	req := database.ScopeRequest{}
	page, err := fetch(cmd.Context(), req)
	if err != nil {
		return config.Scope{}, err
	}
	pos, note := 0, ""
	retained := slices.Clone(initial.Schemas)
	_, noColor := os.LookupEnv("NO_COLOR")
	style := func(code, text string) string {
		if noColor {
			return text
		}
		return "\x1b[" + code + "m" + text + "\x1b[0m"
	}
	render := func() error {
		var out strings.Builder
		out.WriteString("\x1b[2J\x1b[H")
		out.WriteString(style("1;36", "Choose schemas") + "\n")
		mode := "Blacklist · New schemas allowed"
		if selected.Mode == "whitelist" {
			mode = "Whitelist · New schemas blocked"
		}
		out.WriteString(mode + "\n\n")
		if req.Search != "" {
			fmt.Fprintf(&out, "Search: %q\n", req.Search)
		}
		if len(page.Schemas) == 0 {
			out.WriteString("No matching accessible schemas.\n")
		}
		start := max(0, pos-5)
		for i := start; i < min(len(page.Schemas), start+12); i++ {
			mark := "[ ]"
			if selected.ContainsSchema(page.Schemas[i]) {
				mark = style("32", "[x]")
			}
			cursor, name := " ", fmt.Sprintf("%q", page.Schemas[i])
			if i == pos {
				cursor = ">"
				name = style("1;36", name)
			}
			fmt.Fprintf(&out, "%s %s %s\n", cursor, mark, name)
		}
		out.WriteString("\n" + style("2", "↑/↓ Move · Space Toggle · a Toggle all · m Switch mode") + "\n")
		out.WriteString(style("2", "/ Search · Enter Review · q Cancel") + "\n")
		if page.Next != "" {
			out.WriteString(style("2", "n Next page") + "\n")
		}
		if req.After != "" {
			out.WriteString(style("2", "b First page") + "\n")
		}
		if note != "" {
			out.WriteString(style("33", note) + "\n")
		}
		_, err := io.WriteString(f.terminal, out.String())
		return err
	}
	reader := commandInput(cmd)
	key := func() (byte, error) {
		var b [1]byte
		if _, err := io.ReadFull(reader, b[:]); err != nil {
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
				pos = min(max(0, len(page.Schemas)-1), pos+1)
			}
		} else {
			switch k {
			case 3, 4, 'q':
				return config.Scope{}, context.Canceled
			case '\r', '\n':
				return selected, nil
			case 'a', 'm':
				if _, err := io.WriteString(f.terminal, "Loading all schemas…\n"); err != nil {
					return config.Scope{}, failure("cannot render scope selection")
				}
				next, e := bulkScope(cmd.Context(), selected, retained, k == 'm', fetch)
				if e != nil {
					if cmd.Context().Err() != nil {
						return config.Scope{}, cmd.Context().Err()
					}
					note = "Could not load all schemas. Selection unchanged; retry or toggle individual schemas."
				} else {
					for _, name := range append(slices.Clone(selected.Schemas), next.Schemas...) {
						if !slices.Contains(retained, name) {
							retained = append(retained, name)
						}
					}
					selected = next
				}
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
				req.Search, req.After, refresh = search, "", true
			case ' ':
				if len(page.Schemas) == 0 {
					continue
				}
				next := config.Scope{Mode: selected.Mode, Schemas: slices.Clone(selected.Schemas)}
				name := page.Schemas[pos]
				if slices.Contains(next.Schemas, name) {
					next.Schemas = slices.DeleteFunc(next.Schemas, func(v string) bool { return v == name })
				} else {
					next.Schemas = append(next.Schemas, name)
				}
				if !scopeWithinLimit(next) {
					note = "Selection limit reached. Remove a saved schema before adding another."
				} else {
					selected = next
				}
			}
		}
		if refresh {
			page, err = fetch(cmd.Context(), req)
			if err != nil {
				return config.Scope{}, err
			}
			pos = 0
		}
	}
}
