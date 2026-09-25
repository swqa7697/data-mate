package main

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var releaseHeading = regexp.MustCompile(`^## \[([^\]]+)\](?: - [0-9]{4}-[0-9]{2}-[0-9]{2})?$`)
var entry = regexp.MustCompile(`(?m)^[-*] \S`)

func parseVersion(raw string) (string, error) {
	v := strings.TrimSuffix(raw, "\n")
	if !stableVersion.MatchString(v) {
		return "", errors.New("VERSION must contain one stable X.Y.Z version without leading zeroes")
	}
	return v, nil
}

func nextVersion(version, part string) (string, error) {
	if _, err := parseVersion(version); err != nil {
		return "", err
	}
	index := -1
	for i, name := range []string{"major", "minor", "patch"} {
		if part == name {
			index = i
		}
	}
	if index < 0 {
		return "", errors.New("bump requires major, minor, or patch")
	}
	parts := strings.Split(version, ".")
	n, _ := new(big.Int).SetString(parts[index], 10)
	parts[index] = n.Add(n, big.NewInt(1)).String()
	for i := index + 1; i < 3; i++ {
		parts[i] = "0"
	}
	return strings.Join(parts, "."), nil
}

// Reject duplicate release headings before selecting a section. A section ends at
// any level-two heading, so notes cannot silently absorb a following release.
func section(raw, version string, requireEntries bool) (string, error) {
	lines := strings.Split(raw, "\n")
	start, end := -1, len(lines)
	seen := map[string]bool{}
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") && start >= 0 && end == len(lines) {
			end = i
		}
		m := releaseHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if seen[m[1]] {
			return "", fmt.Errorf("duplicate CHANGELOG section [%s]", m[1])
		}
		seen[m[1]] = true
		if m[1] == version {
			start = i + 1
		}
	}
	if start < 0 {
		return "", fmt.Errorf("CHANGELOG has no [%s] section", version)
	}
	text := strings.Trim(strings.Join(lines[start:end], "\n"), "\n")
	if requireEntries && !entry.MatchString(text) {
		return "", fmt.Errorf("CHANGELOG [%s] has no entries", version)
	}
	return text, nil
}

func rollChangelog(raw, version, day string) (string, error) {
	if _, err := section(raw, "Unreleased", true); err != nil {
		return "", err
	}
	for _, line := range strings.Split(raw, "\n") {
		if m := releaseHeading.FindStringSubmatch(line); m != nil && m[1] == version {
			return "", fmt.Errorf("CHANGELOG [%s] already exists", version)
		}
	}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if m := releaseHeading.FindStringSubmatch(line); m != nil && m[1] == "Unreleased" {
			lines[i] = "## [Unreleased]\n\n## [" + version + "] - " + day
			return strings.Join(lines, "\n"), nil
		}
	}
	return "", errors.New("missing Unreleased heading")
}

func (a *app) read(name string) (string, error) {
	path := filepath.Join(a.root, name)
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a regular file", name)
	}
	raw, err := os.ReadFile(path)
	return string(raw), err
}

func (a *app) version() (string, error) {
	raw, err := a.read("VERSION")
	if err != nil {
		return "", err
	}
	return parseVersion(raw)
}

func (a *app) bump(part, day string) error {
	paths, err := a.changed("--", "VERSION", "CHANGELOG.md")
	if err != nil {
		return err
	}
	if len(paths) != 0 {
		return errors.New("release files already modified; commit or stash them before bumping")
	}
	version, err := a.version()
	if err != nil {
		return err
	}
	next, err := nextVersion(version, part)
	if err != nil {
		return err
	}
	raw, err := a.read("CHANGELOG.md")
	if err != nil {
		return err
	}
	rolled, err := rollChangelog(raw, next, day)
	if err != nil {
		return err
	}
	// Prepare both replacements before publishing either file. Restore VERSION if
	// the second rename fails; ordinary validation failures never modify files.
	versionTemp, err := a.prepareFile("VERSION", next)
	if err != nil {
		return err
	}
	defer os.Remove(versionTemp)
	changelogTemp, err := a.prepareFile("CHANGELOG.md", rolled)
	if err != nil {
		return err
	}
	defer os.Remove(changelogTemp)
	oldVersion, err := a.read("VERSION")
	if err != nil {
		return err
	}
	if err = os.Rename(versionTemp, filepath.Join(a.root, "VERSION")); err != nil {
		return err
	}
	if err = os.Rename(changelogTemp, filepath.Join(a.root, "CHANGELOG.md")); err != nil {
		return errors.Join(err, os.WriteFile(filepath.Join(a.root, "VERSION"), []byte(oldVersion), 0644))
	}
	fmt.Fprintf(a.out, "Bumped %s -> %s and rolled CHANGELOG.md. Next: make release-commit on a release branch.\n", version, next)
	return nil
}

func (a *app) prepareFile(name, text string) (string, error) {
	info, err := os.Stat(filepath.Join(a.root, name))
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(a.root, ".release-*")
	if err != nil {
		return "", err
	}
	_, writeErr := f.WriteString(text)
	err = errors.Join(writeErr, f.Chmod(info.Mode().Perm()), f.Close())
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
