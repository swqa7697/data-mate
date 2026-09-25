package distribution

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/swqa7697/data-mate/internal/config"
)

type ShellBlock struct {
	Path    string `json:"path"`
	Text    string `json:"text"`
	Created bool   `json:"created"`
}

// Quote returns a literal POSIX shell word, including embedded apostrophes.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// LoginShell reads the OS account's login shell without trusting SHELL or HOME.
func LoginShell(ctx context.Context) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	raw, err := command(ctx, "/usr/bin/dscl", ".", "-read", "/Users/"+u.Username, "UserShell")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 || fields[0] != "UserShell:" {
		return "", ErrConflict
	}
	return filepath.Base(fields[1]), nil
}

func loader(home, root, shell string) []byte {
	bin := Quote(filepath.Join(home, ".local", "bin"))
	script := Quote(filepath.Join(root, "shell", "completion."+shell))
	// Source loaders preserve caller options and create no completion cache.
	if shell == "zsh" {
		return []byte("# Data Mate shell activation\ncase :$PATH: in\n  *:" + bin + ":*) ;;\n  *) export PATH=" + bin + ":\"$PATH\" ;;\nesac\nif (( ! $+functions[compdef] )); then\n  autoload -Uz compinit\n  compinit -D || return\nfi\nsource " + script + "\n")
	}
	return []byte("# Data Mate shell activation\ncase :$PATH: in\n  *:" + bin + ":*) ;;\n  *) export PATH=" + bin + ":\"$PATH\" ;;\nesac\nsource " + script + "\n")
}

func (e *Engine) shellChanges(old []ShellBlock) ([]desired, []ShellBlock, error) {
	blocks := append([]ShellBlock{}, old...)
	if e.NoShell || (e.Shell != "bash" && e.Shell != "zsh") {
		return nil, blocks, nil
	}
	var paths []string
	if e.Shell == "zsh" {
		dir := e.Home
		if e.ZDotDir != "" {
			if !filepath.IsAbs(e.ZDotDir) || config.CheckPath(e.ZDotDir) != nil {
				return nil, nil, ErrConflict
			}
			dir = e.ZDotDir
		}
		paths = []string{filepath.Join(dir, ".zshrc")}
	} else {
		paths = []string{filepath.Join(e.Home, ".bashrc")}
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
		f, raw, err := inspect(path)
		created := os.IsNotExist(err)
		if err != nil && !created {
			return nil, nil, err
		}
		if f.Target != "" {
			return nil, nil, ErrConflict
		}
		text := string(raw)
		managed := false
		for _, b := range blocks {
			if b.Path != path {
				continue
			}
			if strings.Count(text, b.Text) != 1 {
				return nil, nil, ErrConflict
			}
			managed = true
		}
		if managed {
			continue
		}
		if strings.Contains(text, "# >>> Data Mate") || strings.Contains(text, "# <<< Data Mate") {
			return nil, nil, ErrConflict
		}
		block := "# >>> Data Mate >>>\nsource " + Quote(filepath.Join(e.Root.Path, "shell", "loader."+e.Shell)) + "\n# <<< Data Mate <<<\n"
		if len(text) != 0 && !strings.HasSuffix(text, "\n") {
			block = "\n" + block
		}
		mode := os.FileMode(f.Mode)
		if created {
			mode = 0600
		}
		changes = append(changes, desired{path: path, raw: []byte(text + block), mode: mode, external: true})
		blocks = append(blocks, ShellBlock{path, block, created})
	}
	return changes, blocks, nil
}
