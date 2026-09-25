package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const repository = "swqa7697/data-mate"

var publicAssets = []string{"install.sh", "data-mate_darwin_arm64", "release.txt", "SHA256SUMS"}

type asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	State  string `json:"state"`
}

type githubRelease struct {
	Tag        string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Immutable  bool    `json:"immutable"`
	Target     string  `json:"target_commitish"`
	Assets     []asset `json:"assets"`
}

func (a *app) gh(args ...string) (string, error) { return a.command("", "gh", args...) }

func releaseAssets(dir string) (map[string]asset, error) {
	assets := map[string]asset{}
	for _, name := range publicAssets {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 256<<20 {
			return nil, fmt.Errorf("invalid release asset %s", name)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, f)
		err = errors.Join(err, f.Close())
		if err != nil {
			return nil, err
		}
		assets[name] = asset{Name: name, Size: info.Size(), Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), State: "uploaded"}
	}
	if assets["SHA256SUMS"].Size > 65536 || assets["release.txt"].Size > 4096 {
		return nil, errors.New("oversized release manifest or metadata")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, errors.New("invalid checksum record")
		}
		name := fields[1]
		if name == "SHA256SUMS" || seen[name] || assets[name].Digest != "sha256:"+fields[0] {
			return nil, errors.New("checksum manifest does not match release assets")
		}
		seen[name] = true
	}
	if len(seen) != 3 {
		return nil, errors.New("incomplete checksum manifest")
	}
	return assets, nil
}

func (a *app) releaseList() ([][]githubRelease, error) {
	// The tag endpoint only returns published releases. Authenticated listing
	// includes drafts and must succeed before treating a release as absent.
	raw, err := a.gh("api", "--paginate", "--slurp", "repos/"+repository+"/releases?per_page=100")
	var pages [][]githubRelease
	if err == nil {
		err = json.Unmarshal([]byte(raw), &pages)
	}
	return pages, err
}

func (a *app) releaseInfo(tag string) (githubRelease, error) {
	pages, err := a.releaseList()
	if err != nil {
		return githubRelease{}, err
	}
	for _, page := range pages {
		for _, release := range page {
			if release.Tag == tag {
				return release, nil
			}
		}
	}
	return githubRelease{}, fmt.Errorf("release %s not found in authenticated release listing", tag)
}

func verifyUploaded(release githubRelease, tag string, assets map[string]asset) error {
	// target_commitish is ignored by GitHub when a tag already exists. Verify the
	// actual remote tag separately instead of trusting that informational field.
	if release.Tag != tag || !release.Draft || release.Prerelease || len(release.Assets) != len(assets) {
		return errors.New("draft identity or asset count mismatch")
	}
	seen := map[string]bool{}
	for _, got := range release.Assets {
		if seen[got.Name] || assets[got.Name] != got {
			return fmt.Errorf("uploaded asset verification failed: %s", got.Name)
		}
		seen[got.Name] = true
	}
	return nil
}

func (a *app) remoteTag(tag, head string) error {
	raw, err := a.git("ls-remote", "--tags", "origin", "refs/tags/"+tag, "refs/tags/"+tag+"^{}")
	if err != nil {
		return err
	}
	target, peeled := "", ""
	for line := range strings.SplitSeq(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.HasSuffix(fields[1], "^{}") {
			peeled = fields[0]
		} else {
			target = fields[0]
		}
	}
	if peeled != "" {
		target = peeled
	}
	if target != head {
		return errors.New("remote tag no longer resolves to the validated commit")
	}
	return nil
}

func (a *app) publish(tag, dir string) (err error) {
	head, err := a.validate(tag)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		return errors.New("artifact directory must be absolute")
	}
	assets, err := releaseAssets(dir)
	if err != nil {
		return err
	}
	metadata, err := os.ReadFile(filepath.Join(dir, "release.txt"))
	if err != nil {
		return err
	}
	if !strings.Contains("\n"+string(metadata), "\nversion="+tag[1:]+"\n") {
		return errors.New("artifact version differs from tag")
	}
	if err = a.remoteTag(tag, head); err != nil {
		return err
	}
	pages, err := a.releaseList()
	if err != nil {
		return err
	}
	for _, page := range pages {
		for _, release := range page {
			if release.Tag == tag {
				return fmt.Errorf("release %s already exists (draft=%t); inspect it before retrying; never overwrite published assets", tag, release.Draft)
			}
		}
	}
	changelog, err := a.read("CHANGELOG.md")
	if err != nil {
		return err
	}
	notes, err := section(changelog, tag[1:], true)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("/tmp", "data-mate-release-notes-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(notes + "\n")
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("publication incomplete for %s; inspect GitHub state before retrying. Any draft is left unpublished. To rebuild, explicitly remove only the unpublished draft, preserve the tag, then rerun the workflow; never delete a published release: %w", tag, err)
		}
	}()
	if _, err = a.gh("release", "create", tag, "--repo", repository, "--draft", "--verify-tag", "--target", head, "--title", tag, "--notes-file", f.Name()); err != nil {
		return err
	}
	upload := []string{"release", "upload", tag, "--repo", repository}
	for _, name := range publicAssets {
		upload = append(upload, filepath.Join(dir, name)+"#"+name)
	}
	if _, err = a.gh(upload...); err != nil {
		return err
	}
	release, err := a.releaseInfo(tag)
	if err != nil {
		return err
	}
	if err = verifyUploaded(release, tag, assets); err != nil {
		return err
	}
	if err = a.remoteTag(tag, head); err != nil {
		return err
	}
	// GitHub selects Latest automatically; retrying an old tag must not force it.
	if _, err = a.gh("release", "edit", tag, "--repo", repository, "--draft=false"); err != nil {
		return err
	}
	release, err = a.releaseInfo(tag)
	if err != nil {
		return err
	}
	if release.Draft || release.Prerelease || !release.Immutable || release.Tag != tag {
		return errors.New("published release is not stable and immutable; inspect repository settings and release state")
	}
	fmt.Fprintf(a.out, "Published https://github.com/%s/releases/tag/%s\n", repository, tag)
	return nil
}
