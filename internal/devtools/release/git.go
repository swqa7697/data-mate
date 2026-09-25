package main

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// NUL records preserve whitespace/newlines in filenames and both sides of renames.
func (a *app) changed(extra ...string) ([]string, error) {
	args := append([]string{"status", "--porcelain=v1", "-z", "--untracked-files=all"}, extra...)
	raw, err := a.git(args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	records := strings.Split(raw, "\x00")
	for i := 0; i < len(records); i++ {
		record := records[i]
		if record == "" {
			continue
		}
		if len(record) < 4 {
			return nil, errors.New("invalid git status record")
		}
		paths = append(paths, record[3:])
		if strings.ContainsAny(record[:2], "RC") {
			i++
			if i >= len(records) || records[i] == "" {
				return nil, errors.New("invalid git rename record")
			}
			paths = append(paths, records[i])
		}
	}
	return paths, nil
}

func (a *app) branch() (string, error) {
	raw, err := a.git("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", errors.New("release maintenance requires an attached branch")
	}
	return strings.TrimSpace(raw), nil
}

func (a *app) head() (string, error) {
	raw, err := a.git("rev-parse", "HEAD^{commit}")
	return strings.TrimSpace(raw), err
}

func (a *app) commit(yes bool) error {
	branch, err := a.branch()
	if err != nil {
		return err
	}
	if branch == "main" || branch == "master" {
		return errors.New("release commits require a development or release branch")
	}
	paths, err := a.changed("--untracked-files=no")
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("nothing changed; run make bump-major, bump-minor, or bump-patch")
	}
	for _, path := range paths {
		if path != "VERSION" && path != "CHANGELOG.md" {
			return fmt.Errorf("unrelated tracked or staged change: %q", path)
		}
	}
	version, err := a.version()
	if err != nil {
		return err
	}
	oldRaw, err := a.git("show", "HEAD:VERSION")
	if err != nil {
		return err
	}
	old, err := parseVersion(oldRaw)
	if err != nil {
		return err
	}
	if semver.Compare("v"+version, "v"+old) <= 0 {
		return errors.New("release version must increase relative to HEAD")
	}
	raw, err := a.read("CHANGELOG.md")
	if err != nil {
		return err
	}
	if _, err = section(raw, version, true); err != nil {
		return err
	}
	head, err := a.head()
	if err != nil {
		return err
	}
	remote, err := a.git("ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if remote != "" && strings.Fields(remote)[0] != head {
		return errors.New("HEAD differs from origin branch; the release commit must be the only pushed commit on an existing remote branch")
	}
	subject := "release data-mate: " + version
	diff, err := a.git("diff", "HEAD", "--", "VERSION", "CHANGELOG.md")
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "%s\n%s\n", subject, diff)
	if !yes {
		answer, err := a.confirm("Stage release files, commit, and push origin/" + branch + "? [y/N]: ")
		if err != nil {
			return err
		}
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			return errors.New("aborted")
		}
	}
	if _, err = a.git("add", "--", "VERSION", "CHANGELOG.md"); err != nil {
		return err
	}
	if _, err = a.git("commit", "-m", subject); err != nil {
		return err
	}
	if _, err = a.git("push", "--no-follow-tags", "-u", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return fmt.Errorf("release commit retained locally; resolve the push error and explicitly push branch %q (do not rerun the bump): %w", branch, err)
	}
	fmt.Fprintf(a.out, "Pushed %s. Next: /release-pr (Claude Code) or $release-pr (Codex).\n", subject)
	return nil
}

func (a *app) tag() error {
	if !a.terminal {
		return errors.New("make tag requires an interactive terminal; no bypass is available")
	}
	branch, err := a.branch()
	if err != nil {
		return err
	}
	if branch != "main" {
		return errors.New("release tags must be created on main")
	}
	paths, err := a.changed()
	if err != nil {
		return err
	}
	if len(paths) != 0 {
		return errors.New("release tagging requires a clean checkout")
	}
	if _, err = a.git("fetch", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return err
	}
	head, err := a.head()
	if err != nil {
		return err
	}
	remote, err := a.git("rev-parse", "refs/remotes/origin/main^{commit}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(remote) != head {
		return errors.New("HEAD must equal current origin/main")
	}
	version, err := a.version()
	if err != nil {
		return err
	}
	raw, err := a.read("CHANGELOG.md")
	if err != nil {
		return err
	}
	notes, err := section(raw, version, true)
	if err != nil {
		return err
	}
	name := "v" + version
	local, err := a.git("tag", "--list", name)
	if err != nil {
		return err
	}
	remote, err = a.git("ls-remote", "--tags", "origin")
	if err != nil {
		return err
	}
	if strings.TrimSpace(local) != "" {
		return fmt.Errorf("%s already exists locally", name)
	}
	for line := range strings.SplitSeq(remote, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		remoteTag := strings.TrimSuffix(strings.TrimPrefix(fields[1], "refs/tags/"), "^{}")
		if remoteTag == name {
			return fmt.Errorf("%s already exists on origin", name)
		}
		if strings.HasPrefix(remoteTag, "v") && stableVersion.MatchString(remoteTag[1:]) && fields[0] == head {
			return fmt.Errorf("HEAD already has remote release tag %s", remoteTag)
		}
	}
	local, err = a.git("tag", "--points-at", "HEAD")
	if err != nil {
		return err
	}
	for tag := range strings.SplitSeq(local, "\n") {
		if strings.HasPrefix(tag, "v") && stableVersion.MatchString(tag[1:]) {
			return fmt.Errorf("HEAD already has release tag %s", tag)
		}
	}
	message := "Release " + name + "\n\n" + notes + "\n"
	fmt.Fprintf(a.out, "%s -> %s\n%s\n", name, head, message)
	if err = a.captcha(); err != nil {
		return err
	}
	if _, err = a.command(message, "git", "tag", "-a", name, head, "--cleanup=verbatim", "-F", "-"); err != nil {
		return err
	}
	if _, err = a.git("push", "--no-follow-tags", "origin", "refs/tags/"+name); err != nil {
		return fmt.Errorf("local tag %s retained; resolve the error and explicitly push that tag (do not recreate it): %w", name, err)
	}
	fmt.Fprintf(a.out, "Pushed %s; release.yml will verify, sign, accept, and publish the release.\n", name)
	return nil
}

// validate is shared by hosted validation, signing, and publication. Callers fetch
// origin/main and the exact tag explicitly; no branch-like input is accepted.
func (a *app) validate(tag string) (string, error) {
	if !strings.HasPrefix(tag, "v") || !stableVersion.MatchString(tag[1:]) {
		return "", errors.New("expected stable vMAJOR.MINOR.PATCH tag without leading zeroes")
	}
	version, err := a.version()
	if err != nil {
		return "", err
	}
	if tag != "v"+version {
		return "", errors.New("tag does not match VERSION")
	}
	paths, err := a.changed()
	if err != nil {
		return "", err
	}
	if len(paths) != 0 {
		return "", errors.New("release requires a clean checkout")
	}
	head, err := a.head()
	if err != nil {
		return "", err
	}
	target, err := a.git("rev-parse", "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(target) != head {
		return "", errors.New("tag does not resolve to checked-out commit")
	}
	if _, err = a.git("merge-base", "--is-ancestor", head, "refs/remotes/origin/main"); err != nil {
		return "", errors.New("release commit is not in origin/main history")
	}
	raw, err := a.read("CHANGELOG.md")
	if err != nil {
		return "", err
	}
	if _, err = section(raw, version, true); err != nil {
		return "", err
	}
	return head, nil
}
