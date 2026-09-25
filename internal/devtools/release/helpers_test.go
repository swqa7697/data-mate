package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureChangelog = "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- New behavior.\n\n## [1.2.3] - 2026-09-01\n\n### Fixed\n\n- Previous behavior.\n"

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gitOK(t *testing.T, a *app, args ...string) string {
	t.Helper()
	raw, err := a.git(args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(raw)
}

func fixture(t *testing.T) *app {
	t.Helper()
	// Never inherit signing, hooks, credentials or user Git configuration.
	for key, value := range map[string]string{"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_TERMINAL_PROMPT": "0", "GIT_AUTHOR_NAME": "Release Test", "GIT_AUTHOR_EMAIL": "release@example.invalid", "GIT_COMMITTER_NAME": "Release Test", "GIT_COMMITTER_EMAIL": "release@example.invalid"} {
		t.Setenv(key, value)
	}
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_COUNT"} {
		t.Setenv(key, "")
	}
	// Unset rather than set empty: Git treats an empty GIT_DIR as an override.
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_COUNT"} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	a := &app{ctx: t.Context(), root: root, in: strings.NewReader(""), out: &bytes.Buffer{}, terminal: true, code: func() (string, error) { return "1234", nil }}
	gitOK(t, a, "init", "-b", "main")
	gitOK(t, a, "config", "commit.gpgsign", "false")
	gitOK(t, a, "config", "tag.gpgsign", "false")
	gitOK(t, a, "config", "core.hooksPath", filepath.Join(root, "absent-hooks"))
	writeFile(t, filepath.Join(root, "VERSION"), "1.2.3\n")
	writeFile(t, filepath.Join(root, "CHANGELOG.md"), fixtureChangelog)
	gitOK(t, a, "add", "--", "VERSION", "CHANGELOG.md")
	gitOK(t, a, "commit", "-m", "fixture")
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitOK(t, a, "init", "--bare", remote)
	gitOK(t, a, "remote", "add", "origin", remote)
	gitOK(t, a, "push", "-u", "origin", "main")
	return a
}

func taggedFixture(t *testing.T) *app {
	t.Helper()
	a := fixture(t)
	gitOK(t, a, "tag", "-a", "v1.2.3", "-m", "fixture release")
	gitOK(t, a, "push", "origin", "refs/tags/v1.2.3")
	return a
}

func artifactFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"install.sh": "#!/bin/bash\n", "data-mate_darwin_arm64": "synthetic signed candidate", "release.txt": "format=1\nversion=1.2.3\nplatform=darwin_arm64\nminimum_macos=15.0\nstore_schema=1\ninventory_schema=1\n"}
	var sums strings.Builder
	for _, name := range publicAssets[:3] {
		writeFile(t, filepath.Join(dir, name), files[name])
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256([]byte(files[name])), name)
	}
	writeFile(t, filepath.Join(dir, "SHA256SUMS"), sums.String())
	return dir
}

// The fake GitHub executable records actual subprocess arguments and simulates
// server state. No test needs credentials or contacts GitHub.
func fakeGitHub(t *testing.T, a *app, dir, mode string) string {
	t.Helper()
	fakeDir := t.TempDir()
	assets, err := releaseAssets(dir)
	if err != nil {
		t.Fatal(err)
	}
	draft := githubRelease{Tag: "v1.2.3", Draft: true, Target: gitOK(t, a, "rev-parse", "HEAD")}
	for _, name := range publicAssets {
		draft.Assets = append(draft.Assets, assets[name])
	}
	if mode == "digest" {
		draft.Assets[0].Digest = "sha256:wrong"
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fakeDir, "draft.json"), string(raw))
	draft.Draft, draft.Immutable = false, true
	raw, err = json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fakeDir, "published.json"), string(raw))
	writeFile(t, filepath.Join(fakeDir, "list.json"), "[[]]")
	if mode == "existing" {
		writeFile(t, filepath.Join(fakeDir, "list.json"), `[[{"tag_name":"v1.2.3","draft":false}]]`)
	}
	if mode == "draft" {
		writeFile(t, filepath.Join(fakeDir, "list.json"), `[[{"tag_name":"v1.2.3","draft":true}]]`)
	}
	script := `#!/bin/bash
set -euo pipefail
base="$(dirname "$0")"
printf '%s\n' "$*" >> "$base/calls"
case "$1 $2" in
  'api --paginate')
    [[ "$FAKE_GH_MODE" != list-error ]] || exit 1
    if [[ -e "$base/created" ]]; then
      [[ "$FAKE_GH_MODE" != draft-list-error ]] || exit 1
      if [[ "$FAKE_GH_MODE" == missing-draft ]]; then
        printf '[[]]'
        exit 0
      fi
      printf '[[{"tag_name":"v0.0.1"}],['
      if [[ -e "$base/published" ]]; then cat "$base/published.json"; else cat "$base/draft.json"; fi
      printf ']]'
    else
      cat "$base/list.json"
    fi ;;
  'release create')
    [[ "$*" == *'--draft --verify-tag'* && "$*" == *'--notes-file /tmp/'* ]]
    touch "$base/created" ;;
  'release upload')
    [[ "$FAKE_GH_MODE" != upload-error ]] || exit 1
    [[ "$*" != *--clobber* ]]
    [[ "$#" == 9 ]] ;;
  'release edit') touch "$base/published" ;;
  'api repos/swqa7697/data-mate/releases/tags/v1.2.3')
    # GitHub's tag endpoint only returns published releases, never drafts.
    if [[ ! -e "$base/published" ]]; then
      printf 'gh: Not Found (HTTP 404)\n' >&2
      exit 1
    fi
    cat "$base/published.json" ;;
  *) exit 2 ;;
esac
`
	path := filepath.Join(fakeDir, "gh")
	writeFile(t, path, script)
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_MODE", mode)
	return fakeDir
}

func requireFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("operation unexpectedly succeeded")
	}
}

func snapshot(t *testing.T, a *app) string {
	t.Helper()
	return gitOK(t, a, "status", "--porcelain=v1") + "\n" + gitOK(t, a, "diff", "HEAD") + "\n" + gitOK(t, a, "rev-parse", "HEAD") + "\n" + gitOK(t, a, "show-ref")
}
