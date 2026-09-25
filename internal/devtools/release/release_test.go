package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Existing devtools scenarios own installation/cleanup; distribution regressions
// own downloaded runtime artifacts. Neither exercises release-author Git state or
// GitHub publication. These command scenarios protect those new boundaries.
func TestReleasePreparation(t *testing.T) {
	for _, tc := range []struct{ part, version string }{{"major", "2.0.0"}, {"minor", "1.3.0"}, {"patch", "1.2.4"}} {
		t.Run(tc.part, func(t *testing.T) {
			a := fixture(t)
			gitOK(t, a, "switch", "-c", "release/"+tc.version)
			// A user's push.followTags must not publish other local release tags.
			gitOK(t, a, "config", "push.followTags", "true")
			gitOK(t, a, "tag", "-a", "v0.9.0", "-m", "unrelated tag")
			if err := a.bump(tc.part, "2026-09-24"); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(a.root, "VERSION")); got != tc.version {
				t.Fatal(got)
			}
			rolled := readFile(t, filepath.Join(a.root, "CHANGELOG.md"))
			want := strings.Replace(fixtureChangelog, "## [Unreleased]", "## [Unreleased]\n\n## ["+tc.version+"] - 2026-09-24", 1)
			if rolled != want {
				t.Fatal("bump changed notes or spacing", rolled)
			}
			before := snapshot(t, a)
			a.in = strings.NewReader("n\n")
			requireFailure(t, a.execute([]string{"commit"}))
			if snapshot(t, a) != before {
				t.Fatal("declined commit mutated Git state")
			}
			a.in = strings.NewReader("yes\n")
			if err := a.execute([]string{"commit"}); err != nil {
				t.Fatal(err)
			}
			// Release preparation follows Arisu's dedicated release subject,
			// rather than the Conventional Commit format for ordinary changes.
			if got := gitOK(t, a, "log", "-1", "--format=%s"); got != "release data-mate: "+tc.version {
				t.Fatal(got)
			}
			if got := gitOK(t, a, "diff", "--name-only", "HEAD^", "HEAD"); got != "CHANGELOG.md\nVERSION" {
				t.Fatal("wrong commit files", got)
			}
			if gitOK(t, a, "rev-parse", "HEAD") != gitOK(t, a, "rev-parse", "origin/release/"+tc.version) {
				t.Fatal("release commit not pushed")
			}
			if got := gitOK(t, a, "ls-remote", "--tags", "origin"); got != "" {
				t.Fatal("release commit unexpectedly pushed tags", got)
			}
		})
	}
}

func TestPreparationRefusalsPreserveWork(t *testing.T) {
	for _, scenario := range []string{"dirty-release", "invalid-version", "empty-notes", "duplicate-section", "already-released", "unrelated-staged", "renamed-file", "main", "master", "detached", "remote-differs", "version-not-increased", "noninteractive"} {
		t.Run(scenario, func(t *testing.T) {
			a := fixture(t)
			operation := []string{"bump", "patch"}
			switch scenario {
			case "dirty-release":
				writeFile(t, filepath.Join(a.root, "VERSION"), "1.2.5\n")
			case "invalid-version", "empty-notes", "duplicate-section", "already-released":
				switch scenario {
				case "invalid-version":
					writeFile(t, filepath.Join(a.root, "VERSION"), "01.2.3\n")
				case "empty-notes":
					writeFile(t, filepath.Join(a.root, "CHANGELOG.md"), "## [Unreleased]\n\n### Added\n")
				case "duplicate-section":
					writeFile(t, filepath.Join(a.root, "CHANGELOG.md"), fixtureChangelog+"\n## [Unreleased]\n")
				case "already-released":
					writeFile(t, filepath.Join(a.root, "CHANGELOG.md"), fixtureChangelog+"\n## [1.2.4]\n\n- Already released.\n")
				}
				gitOK(t, a, "add", "--", "VERSION", "CHANGELOG.md")
				gitOK(t, a, "commit", "-m", "invalid fixture")
			default:
				gitOK(t, a, "switch", "-c", "release/1.2.4")
				if err := a.bump("patch", "2026-09-24"); err != nil {
					t.Fatal(err)
				}
				operation = []string{"commit", "--yes"}
				switch scenario {
				case "unrelated-staged":
					writeFile(t, filepath.Join(a.root, "unrelated file"), "keep")
					gitOK(t, a, "add", "--", "unrelated file")
				case "renamed-file":
					gitOK(t, a, "mv", "CHANGELOG.md", "renamed changelog")
				case "main":
					gitOK(t, a, "switch", "main")
				case "master":
					gitOK(t, a, "switch", "-c", "master")
				case "detached":
					gitOK(t, a, "checkout", "--detach")
				case "remote-differs":
					gitOK(t, a, "push", "origin", "HEAD:refs/heads/release/1.2.4")
					gitOK(t, a, "commit", "--allow-empty", "-m", "unrelated local commit")
				case "version-not-increased":
					writeFile(t, filepath.Join(a.root, "VERSION"), "1.2.3\n")
				case "noninteractive":
					a.terminal = false
					operation = []string{"commit"}
				}
			}
			before := snapshot(t, a)
			requireFailure(t, a.execute(operation))
			if snapshot(t, a) != before {
				t.Fatal("refusal changed files, index or refs")
			}
		})
	}
}

func TestTagAndReleaseValidation(t *testing.T) {
	for _, scenario := range []string{"success", "captcha", "noninteractive", "dirty", "wrong-branch", "stale-main", "existing-local", "existing-remote", "already-tagged"} {
		t.Run(scenario, func(t *testing.T) {
			a := fixture(t)
			a.in = strings.NewReader("1234\n")
			switch scenario {
			case "captcha":
				a.in = strings.NewReader("9999\n")
			case "noninteractive":
				a.terminal = false
			case "dirty":
				writeFile(t, filepath.Join(a.root, "untracked"), "keep")
			case "wrong-branch":
				gitOK(t, a, "switch", "-c", "dev")
			case "stale-main":
				gitOK(t, a, "commit", "--allow-empty", "-m", "ahead")
			case "existing-local":
				gitOK(t, a, "tag", "v1.2.3")
			case "existing-remote":
				gitOK(t, a, "tag", "v1.2.3")
				gitOK(t, a, "push", "origin", "refs/tags/v1.2.3")
				gitOK(t, a, "tag", "-d", "v1.2.3")
			case "already-tagged":
				gitOK(t, a, "tag", "-a", "v1.2.2", "-m", "existing")
				gitOK(t, a, "push", "origin", "refs/tags/v1.2.2")
				gitOK(t, a, "tag", "-d", "v1.2.2")
			}
			before := snapshot(t, a)
			err := a.execute([]string{"tag"})
			if scenario != "success" {
				requireFailure(t, err)
				if snapshot(t, a) != before {
					t.Fatal("refused tag mutated checkout or refs")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gitOK(t, a, "cat-file", "-t", "refs/tags/v1.2.3") != "tag" {
				t.Fatal("tag is not annotated")
			}
			if got := gitOK(t, a, "for-each-ref", "--format=%(contents)", "refs/tags/v1.2.3"); got != "Release v1.2.3\n\n### Fixed\n\n- Previous behavior." {
				t.Fatal(got)
			}
			if err := a.remoteTag("v1.2.3", gitOK(t, a, "rev-parse", "HEAD")); err != nil {
				t.Fatal(err)
			}
			if err := a.execute([]string{"validate", "v1.2.3"}); err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []string{"main", "v01.2.3", "v1.2.3-rc.1", "v1.2.3+build", "v1.2.4", "v1.2.3\n"} {
				requireFailure(t, a.execute([]string{"validate", invalid}))
			}
			gitOK(t, a, "switch", "-c", "unmerged")
			gitOK(t, a, "commit", "--allow-empty", "-m", "unmerged")
			gitOK(t, a, "tag", "-f", "v1.2.3")
			requireFailure(t, a.execute([]string{"validate", "v1.2.3"}))
		})
	}
}

func TestPushFailuresRetainRecoveryState(t *testing.T) {
	for _, operation := range []string{"commit", "tag"} {
		t.Run(operation, func(t *testing.T) {
			a := fixture(t)
			remote := gitOK(t, a, "remote", "get-url", "origin")
			writeFile(t, filepath.Join(remote, "hooks", "pre-receive"), "#!/bin/sh\nexit 1\n")
			if err := os.Chmod(filepath.Join(remote, "hooks", "pre-receive"), 0700); err != nil {
				t.Fatal(err)
			}
			before := gitOK(t, a, "rev-parse", "HEAD")
			args := []string{"tag"}
			a.in = strings.NewReader("1234\n")
			if operation == "commit" {
				gitOK(t, a, "switch", "-c", "release/1.2.4")
				if err := a.bump("patch", "2026-09-24"); err != nil {
					t.Fatal(err)
				}
				args = []string{"commit", "--yes"}
			}
			err := a.execute(args)
			if err == nil || !strings.Contains(err.Error(), "retained") {
				t.Fatal("missing recovery diagnostic", err)
			}
			if operation == "tag" {
				gitOK(t, a, "rev-parse", "refs/tags/v1.2.3")
			} else if gitOK(t, a, "rev-parse", "HEAD") == before {
				t.Fatal("lost local release commit")
			}
		})
	}
}

func TestPublicationPreservesDraftUntilAssetsVerify(t *testing.T) {
	// Drafts are absent from the tag endpoint. Verify through authenticated
	// pagination, and preserve the draft if that lookup fails or loses the tag.
	for _, scenario := range []string{"success", "existing", "draft", "list-error", "upload-error", "draft-list-error", "missing-draft", "digest", "checksum", "moved-tag"} {
		t.Run(scenario, func(t *testing.T) {
			a := taggedFixture(t)
			dir := artifactFixture(t)
			fake := fakeGitHub(t, a, dir, scenario)
			switch scenario {
			case "checksum":
				writeFile(t, filepath.Join(dir, "data-mate_darwin_arm64"), "corrupted")
			case "moved-tag":
				remote := gitOK(t, a, "remote", "get-url", "origin")
				gitOK(t, a, "--git-dir="+remote, "update-ref", "-d", "refs/tags/v1.2.3")
			}
			err := a.execute([]string{"publish", "v1.2.3", dir})
			_, publishedErr := os.Stat(filepath.Join(fake, "published"))
			if scenario == "success" {
				if err != nil || publishedErr != nil {
					t.Fatal("publication failed", err, publishedErr)
				}
				calls := readFile(t, filepath.Join(fake, "calls"))
				if strings.Index(calls, "release upload") > strings.Index(calls, "release edit") {
					t.Fatal("published before upload")
				}
				if strings.Contains(calls, "notarization") {
					t.Fatal("uploaded signing diagnostics")
				}
			} else {
				requireFailure(t, err)
				if !os.IsNotExist(publishedErr) {
					t.Fatal("published despite failed prerequisite")
				}
				if scenario == "upload-error" || scenario == "draft-list-error" || scenario == "missing-draft" || scenario == "digest" {
					if _, err := os.Stat(filepath.Join(fake, "created")); err != nil {
						t.Fatal("draft was not retained", err)
					}
				} else if _, err := os.Stat(filepath.Join(fake, "created")); !os.IsNotExist(err) {
					t.Fatal("created a draft despite failed preflight")
				}
			}
		})
	}
}
