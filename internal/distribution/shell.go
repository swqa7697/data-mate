package distribution

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type ShellBlock struct {
	Path    string `json:"path"`
	Text    string `json:"text,omitempty"`
	Created bool   `json:"created,omitempty"`
}

// Quote returns a literal POSIX shell word, including embedded apostrophes.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func loader(home, root, shell string) []byte {
	bin := Quote(filepath.Join(home, ".local", "bin"))
	script := Quote(filepath.Join(root, "shell", "completion."+shell))
	// Source loaders preserve caller options and create no completion cache.
	if shell == "zsh" {
		return []byte("# Data Mate shell activation\ncase :$PATH: in\n  *:" + bin + ":*) ;;\n  *) export PATH=" + bin + ":\"$PATH\" ;;\nesac\nif (( ! $+functions[compdef] )); then\n  autoload -Uz compinit\n  compinit -D || return\nfi\n[ ! -r " + script + " ] || source " + script + "\n")
	}
	return []byte("# Data Mate shell activation\ncase :$PATH: in\n  *:" + bin + ":*) ;;\n  *) export PATH=" + bin + ":\"$PATH\" ;;\nesac\n[ ! -r " + script + " ] || source " + script + "\n")
}

// Only startup files are valid external cleanup targets, including ZDOTDIR.
func validShellPath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	switch filepath.Base(path) {
	case ".bashrc", ".bash_profile", ".bash_login", ".profile", ".zshrc":
		return true
	}
	return false
}
func readShell(path string) ([]byte, os.FileMode, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, 0600, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !st.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("shell startup file %q is not a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if len(raw) > 1<<20 {
		return nil, 0, fmt.Errorf("shell startup file %q is too large", path)
	}
	return raw, st.Mode().Perm(), err
}
func writeShell(path string, raw []byte, mode os.FileMode) error {
	// Resolve the startup file, preserving its original symlink and target mode.
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !os.IsNotExist(err) || !absent(path) {
			return err
		}
		target = path
	}
	if !absent(target) {
		f, err := os.OpenFile(target, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
	}
	return writeAtomic(target, raw, mode)
}

const blockStart = "# >>> Data Mate >>>\n"
const blockEnd = "# <<< Data Mate <<<\n"

// A single pair of complete marker lines owns the block. Unbalanced or duplicate
// markers are ambiguous and preserved, with a warning instead of an install error.
func replaceBlock(text, block string) (string, error) {
	if !strings.Contains(text, "# >>> Data Mate") && !strings.Contains(text, "# <<< Data Mate") {
		if block != "" && text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		return text + block, nil
	}
	start := strings.Index(text, blockStart)
	end := strings.Index(text, blockEnd)
	if start < 0 || end < start || (start > 0 && text[start-1] != '\n') || strings.Count(text, "# >>> Data Mate") != 1 || strings.Count(text, "# <<< Data Mate") != 1 || (end > 0 && text[end-1] != '\n') {
		return "", fmt.Errorf("ambiguous Data Mate shell markers; startup file preserved")
	}
	return text[:start] + block + text[end+len(blockEnd):], nil
}
func (e *Engine) shellChanges(old []ShellBlock) ([]desired, []ShellBlock, error) {
	blocks := append([]ShellBlock{}, old...)
	if e.NoShell {
		return nil, blocks, nil
	}
	if e.Shell != "bash" && e.Shell != "zsh" {
		return nil, blocks, fmt.Errorf("shell integration unavailable for %q; add %s to PATH", e.Shell, filepath.Dir(e.commandPath()))
	}
	paths := []string{}
	if e.Shell == "zsh" {
		dir := e.Home
		if e.ZDotDir != "" {
			if !filepath.IsAbs(e.ZDotDir) {
				return nil, blocks, fmt.Errorf("ZDOTDIR must be an absolute path")
			}
			dir = e.ZDotDir
		}
		paths = append(paths, filepath.Join(dir, ".zshrc"))
	} else {
		paths = append(paths, filepath.Join(e.Home, ".bashrc"))
		login := filepath.Join(e.Home, ".bash_profile")
		for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
			p := filepath.Join(e.Home, name)
			if !absent(p) {
				login = p
				break
			}
		}
		paths = append(paths, login)
	}
	changes := []desired{}
	for _, path := range paths {
		raw, mode, err := readShell(path)
		if err != nil {
			e.warning(err)
			continue
		}
		script := Quote(filepath.Join(e.Root.Path, "shell", "loader."+e.Shell))
		block := blockStart + "[ ! -r " + script + " ] || source " + script + "\n" + blockEnd
		text, err := replaceBlock(string(raw), block)
		if err != nil {
			e.warning(fmt.Errorf("shell integration at %q: %w", path, err))
			continue
		}
		changes = append(changes, desired{path: path, raw: []byte(text), mode: mode})
		found := false
		for i, b := range blocks {
			if b.Path == path {
				blocks[i] = ShellBlock{Path: path}
				found = true
			}
		}
		if !found {
			blocks = append(blocks, ShellBlock{Path: path})
		}
	}
	return changes, blocks, nil
}
func (e *Engine) removeShell(blocks []ShellBlock) {
	for _, b := range blocks {
		if !validShellPath(b.Path) {
			e.warning(fmt.Errorf("ignored invalid shell cleanup path %q", b.Path))
			continue
		}
		raw, mode, err := readShell(b.Path)
		if err != nil {
			e.warning(err)
			continue
		}
		text, err := replaceBlock(string(raw), "")
		if err != nil {
			e.warning(fmt.Errorf("shell cleanup at %q: %w", b.Path, err))
			continue
		}
		if text != string(raw) {
			e.warning(writeShell(b.Path, []byte(text), mode))
		}
	}
}
