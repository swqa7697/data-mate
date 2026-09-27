package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
	"github.com/spf13/cobra"
	"github.com/swqa7697/data-mate/internal/database"
	"golang.org/x/term"
)

func writeDescription(cmd *cobra.Command, description *database.DatabaseDescription) error {
	width := 80
	out, isFile := cmd.OutOrStdout().(*os.File)
	terminal := isFile && term.IsTerminal(int(out.Fd()))
	if terminal {
		if columns, _, err := term.GetSize(int(out.Fd())); err == nil && columns > 0 {
			width = columns
		}
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	text := renderDescription(description, width, terminal && !noColor)
	in, inputFile := cmd.InOrStdin().(*os.File)
	if terminal && inputFile && term.IsTerminal(int(in.Fd())) && !flag(cmd, "no-pager") {
		return pageDescription(cmd, in, text)
	}
	_, err := io.WriteString(cmd.OutOrStdout(), text)
	return err
}

func renderDescription(description *database.DatabaseDescription, width int, color bool) string {
	labels := [...]string{"Enum", "Table", "View", "Sequence", "Index", "Function"}
	headings := [...]string{"Enums", "Tables", "Views", "Sequences", "Indexes", "Functions"}
	tints := [...]string{"\x1b[35m", "\x1b[32m", "\x1b[36m", "\x1b[33m", "\x1b[34m", "\x1b[31m"}
	var out strings.Builder
	fmt.Fprintf(&out, "%s — database=%s\n", displayName(description.Alias), displayName(description.Database))
	if len(description.Schemas) == 0 {
		out.WriteString("\nNo application schemas.\n")
		return out.String()
	}
	if color {
		out.WriteString("\x1b[1mSchema\x1b[0m")
		for i, label := range labels {
			out.WriteString("  ")
			out.WriteString(tints[i] + label + "\x1b[0m")
		}
		out.WriteByte('\n')
	}
	for _, schema := range description.Schemas {
		name := displayName(schema.Name)
		if color {
			name = "\x1b[1m" + name + "\x1b[0m"
		}
		fmt.Fprintf(&out, "\n%s\n", name)
		groups := [6][]string{}
		for _, item := range schema.Enums {
			groups[0] = append(groups[0], item.Name)
		}
		for _, item := range schema.Tables {
			group := 1
			if item.Kind == "view" || item.Kind == "materialized_view" {
				group = 2
			}
			groups[group] = append(groups[group], item.Name)
		}
		for _, item := range schema.Sequences {
			groups[3] = append(groups[3], item.Name)
		}
		for _, item := range schema.Indexes {
			groups[4] = append(groups[4], item.Name)
		}
		for _, item := range schema.Functions {
			groups[5] = append(groups[5], item.Name)
		}
		populated := false
		for i, names := range groups {
			if len(names) == 0 {
				continue
			}
			if populated {
				out.WriteByte('\n')
			}
			populated = true
			if !color {
				fmt.Fprintf(&out, "  %s\n", headings[i])
			}
			sort.Strings(names)
			for j := range names {
				names[j] = displayName(names[j])
			}
			tint := ""
			if color {
				tint = tints[i]
			}
			writeNameGrid(&out, names, width, tint)
		}
		if !populated {
			out.WriteString("  (no objects)\n")
		}
	}
	return out.String()
}

// Names are display text, not SQL identifiers. Preserve printable characters
// without delimiters while preventing catalog names from controlling the terminal.
func displayName(name string) string {
	var out strings.Builder
	for _, r := range name {
		if unicode.IsPrint(r) {
			out.WriteRune(r)
		} else {
			quoted := strconv.QuoteRune(r)
			out.WriteString(quoted[1 : len(quoted)-1])
		}
	}
	return out.String()
}

func writeNameGrid(out *strings.Builder, names []string, width int, tint string) {
	const indent = "  "
	widths := make([]int, len(names))
	for i, name := range names {
		widths[i] = uniseg.StringWidth(name)
	}
	// A row-major grid with per-column widths packs mixed-length names without
	// truncation. Bound candidate columns by the terminal width, not catalog size.
	columns := min(len(names), max(1, width/2))
	var cells []int
	for ; columns > 0; columns-- {
		cells = make([]int, columns)
		for i, size := range widths {
			cells[i%columns] = max(cells[i%columns], size)
		}
		total := len(indent) + 2*(columns-1)
		for _, size := range cells {
			total += size
		}
		if total <= width || columns == 1 {
			break
		}
	}
	for i, name := range names {
		column := i % columns
		if column == 0 {
			out.WriteString(indent)
		}
		out.WriteString(tint)
		out.WriteString(name)
		if tint != "" {
			out.WriteString("\x1b[0m")
		}
		if column == columns-1 || i == len(names)-1 {
			out.WriteByte('\n')
		} else {
			out.WriteString(strings.Repeat(" ", cells[column]-widths[i]+2))
		}
	}
}
