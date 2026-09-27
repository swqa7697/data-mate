// Package distribution owns verified production acquisition and artifact publication.
package distribution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/contracts"
	"golang.org/x/mod/semver"
)

const (
	Repository     = "swqa7697/data-mate"
	TeamID         = "JDNRPL924Q"
	CodeIdentifier = "io.github.swqa7697.data-mate"
	MinimumMacOS   = "15.0"
	MaxBinary      = 256 << 20
)

var ErrRelease = errors.New("release verification failed; installation preserved")
var ErrConflict = errors.New("distribution ownership conflict; preserve state and retry after reconciliation")
var stable = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var osVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// Metadata describes the compiled candidate contract and bounded release.txt.
type Metadata struct {
	Format          int    `json:"format"`
	Version         string `json:"version"`
	Platform        string `json:"platform"`
	MinimumMacOS    string `json:"minimum_macos,omitempty"`
	MinimumGlibc    string `json:"minimum_glibc,omitempty"`
	StoreSchema     int    `json:"store_schema"`
	InventorySchema int    `json:"inventory_schema"`
}

// Contract derives the sole application version from build metadata.
func Contract(version string) Metadata {
	m := Metadata{Format: 1, Version: version, Platform: runtime.GOOS + "_" + runtime.GOARCH, StoreSchema: 1, InventorySchema: 1}
	if runtime.GOOS == "linux" {
		m.MinimumGlibc = "2.39"
	} else {
		m.MinimumMacOS = MinimumMacOS
	}
	return m
}
func (m Metadata) valid() bool {
	platform := (m.Platform == "darwin_arm64" && osVersion.MatchString(m.MinimumMacOS) && m.MinimumGlibc == "") || (m.Platform == "linux_amd64" && osVersion.MatchString(m.MinimumGlibc) && m.MinimumMacOS == "")
	return m.Format == 1 && stable.MatchString(m.Version) && platform && m.StoreSchema > 0 && m.InventorySchema > 0
}

// ParseMetadata never evaluates metadata as shell or accepts extension fields.
func ParseMetadata(raw []byte) (Metadata, error) {
	var m Metadata
	if len(raw) == 0 || len(raw) > 4096 || raw[len(raw)-1] != '\n' {
		return m, ErrRelease
	}
	for _, b := range raw {
		if b != '\n' && (b < 32 || b > 126) {
			return m, ErrRelease
		}
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(raw[:len(raw)-1]), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || v == "" || fields[k] != "" {
			return m, ErrRelease
		}
		fields[k] = v
	}
	if len(fields) != 6 {
		return m, ErrRelease
	}
	integer := func(k string) int {
		value := fields[k]
		n, err := strconv.Atoi(value)
		if err != nil || strconv.Itoa(n) != value {
			return 0
		}
		return n
	}
	m = Metadata{Format: integer("format"), Version: fields["version"], Platform: fields["platform"], MinimumMacOS: fields["minimum_macos"], MinimumGlibc: fields["minimum_glibc"], StoreSchema: integer("store_schema"), InventorySchema: integer("inventory_schema")}
	if !m.valid() {
		return Metadata{}, ErrRelease
	}
	return m, nil
}

// Text is the canonical bootstrap-readable representation.
func (m Metadata) Text() string {
	key, minimum := "minimum_macos", m.MinimumMacOS
	if m.Platform == "linux_amd64" {
		key, minimum = "minimum_glibc", m.MinimumGlibc
	}
	return fmt.Sprintf("format=%d\nversion=%s\nplatform=%s\n%s=%s\nstore_schema=%d\ninventory_schema=%d\n", m.Format, m.Version, m.Platform, key, minimum, m.StoreSchema, m.InventorySchema)
}

func allowedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return false
	}
	return u.Hostname() != ""
}

// Client has a narrow HTTP seam; production uses bounded HTTPS with standard proxy support.
type Client struct {
	HTTP   *http.Client
	verify func(context.Context, string, Metadata) error
}

func NewClient() Client {
	return Client{HTTP: &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, MaxResponseHeaderBytes: 64 << 10}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !allowedURL(req.URL.String()) {
			return ErrRelease
		}
		return nil
	}}}
}
func (c Client) fetch(ctx context.Context, address string, limit int64, out io.Writer) error {
	if !allowedURL(address) {
		return ErrRelease
	}
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return ErrRelease
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return ErrRelease
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.ContentLength > limit {
		return ErrRelease
	}
	n, err := io.Copy(out, io.LimitReader(res.Body, limit+1))
	if err != nil || n > limit || (res.ContentLength >= 0 && n != res.ContentLength) {
		return ErrRelease
	}
	return nil
}

// Candidate is privately staged, verified code. Close removes download staging.
type Candidate struct {
	Path     string
	Metadata Metadata
	dir      string
}

func (c *Candidate) Close() error {
	if c.dir == "" {
		return nil
	}
	return os.RemoveAll(c.dir)
}

// Latest pins every asset to a single stable API release before acquisition.
func (c Client) Latest(ctx context.Context, current string) (_ *Candidate, result error) {
	defer func() {
		if ctx.Err() != nil {
			result = ctx.Err()
		}
	}()
	var api strings.Builder
	if err := c.fetch(ctx, "https://api.github.com/repos/"+Repository+"/releases/latest", 1<<20, &api); err != nil {
		return nil, err
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	// GitHub's API has extension fields; bounded JSON rejects duplicates before decode.
	raw, err := boundedJSON([]byte(api.String()))
	if err != nil || json.Unmarshal(raw, &release) != nil {
		return nil, ErrRelease
	}
	version := strings.TrimPrefix(release.Tag, "v")
	if release.Tag != "v"+version || !stable.MatchString(version) || release.Draft || release.Prerelease || (stable.MatchString(current) && semver.Compare("v"+version, "v"+current) < 0) {
		return nil, ErrRelease
	}
	base := "https://github.com/" + Repository + "/releases/download/" + release.Tag + "/"
	assets := map[string]string{}
	for _, a := range release.Assets {
		if _, ok := assets[a.Name]; ok {
			return nil, ErrRelease
		}
		assets[a.Name] = a.URL
	}
	names := platformAssets(runtime.GOOS + "_" + runtime.GOARCH)
	for _, name := range names.all() {
		if assets[name] != base+name {
			return nil, ErrRelease
		}
	}
	dir, err := os.MkdirTemp("", "data-mate-download-")
	if err != nil {
		return nil, ErrRelease
	}
	candidate := &Candidate{dir: dir, Path: filepath.Join(dir, "data-mate")}
	defer func() {
		if result != nil || ctx.Err() != nil {
			_ = candidate.Close()
		}
	}()
	var meta, sums strings.Builder
	if c.fetch(ctx, base+names.metadata, 4096, &meta) != nil || c.fetch(ctx, base+names.sums, 65536, &sums) != nil {
		return nil, ErrRelease
	}
	candidate.Metadata, err = ParseMetadata([]byte(meta.String()))
	if err != nil || candidate.Metadata.Version != version || candidate.Metadata.Platform != runtime.GOOS+"_"+runtime.GOARCH {
		return nil, ErrRelease
	}
	hashes, err := checksumsFor(sums.String(), names)
	if err != nil || digest([]byte(meta.String())) != hashes[names.metadata] {
		return nil, ErrRelease
	}
	file, err := os.OpenFile(candidate.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return nil, ErrRelease
	}
	err = c.fetch(ctx, base+names.binary, MaxBinary, file)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err != nil || syncErr != nil || closeErr != nil {
		return nil, ErrRelease
	}
	hash, err := hashFile(candidate.Path)
	if err != nil || hash != hashes[names.binary] {
		return nil, ErrRelease
	}
	if names.signature != "" {
		var sig bytes.Buffer
		if c.fetch(ctx, base+names.signature, 4096, &sig) != nil || digest(sig.Bytes()) != hashes[names.signature] || os.WriteFile(candidate.Path+".sig", sig.Bytes(), 0600) != nil {
			return nil, ErrRelease
		}
	}
	verify := c.verify
	if verify == nil {
		verify = VerifyNative
	}
	if err = verify(ctx, candidate.Path, candidate.Metadata); err != nil {
		return nil, err
	}
	var compiled Metadata
	output, err := command(ctx, candidate.Path, "__release-metadata")
	if err != nil || config.DecodeStrict(output, 4096, &compiled) != nil || compiled != candidate.Metadata {
		return nil, ErrRelease
	}
	return candidate, nil
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxBinary+1))
	if err != nil || n > MaxBinary {
		return "", ErrRelease
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type releaseAssets struct{ binary, metadata, sums, signature string }

func platformAssets(platform string) releaseAssets {
	if platform == "linux_amd64" {
		return releaseAssets{"data-mate_linux_amd64", "release_linux_amd64.txt", "SHA256SUMS_linux_amd64", "data-mate_linux_amd64.sig"}
	}
	return releaseAssets{"data-mate_darwin_arm64", "release.txt", "SHA256SUMS", ""}
}
func (a releaseAssets) all() []string {
	names := append(a.hashed(), a.sums)
	return names
}
func (a releaseAssets) hashed() []string {
	names := []string{"install.sh", a.metadata, a.binary}
	if a.signature != "" {
		names = append(names, a.signature)
	}
	return names
}
func checksumsFor(raw string, assets releaseAssets) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		h, name, ok := strings.Cut(line, "  ")
		decoded, err := hex.DecodeString(h)
		if !ok || err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != h || out[name] != "" {
			return nil, ErrRelease
		}
		allowed := false
		for _, asset := range assets.hashed() {
			if name == asset {
				allowed = true
			}
		}
		if !allowed {
			return nil, ErrRelease
		}
		out[name] = h
	}
	if len(out) != len(assets.hashed()) {
		return nil, ErrRelease
	}
	return out, nil
}

type boundedOutput struct{ strings.Builder }

func (w *boundedOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 65536 {
		return 0, ErrRelease
	}
	return w.Builder.Write(p)
}
func command(parent context.Context, exe string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	cmd.WaitDelay = time.Second
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrRelease
	}
	return []byte(out.String()), nil
}

// VerifyNative checks publisher identity and platform compatibility before executing code.
// Notarization is checked during release publication; macOS owns runtime policy.
// Bare Mach-O tools are not assessed as application bundles by spctl.
// Keep the publisher requirement independent of Apple's online notarization
// service. macOS may still enforce its own certificate and execution policies.
func verifyCodeSignature(ctx context.Context, path string, run func(context.Context, string, ...string) ([]byte, error)) error {
	requirement := `=anchor apple generic and identifier "` + CodeIdentifier + `" and certificate leaf[subject.OU] = "` + TeamID + `" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists`
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, err := run(ctx, "/usr/bin/codesign", "--verify", "--strict", "-R", requirement, path)
	return err
}

func boundedJSON(raw []byte) ([]byte, error) { return contracts.JSON(bytes.NewReader(raw), 1<<20) }
