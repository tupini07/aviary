package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// defaultRepo is the GitHub "owner/name" whose releases `aviary update` pulls
// from. Kept as a const so forks can repoint it in one place.
const defaultRepo = "tupini07/aviary"

// defaultGitHubAPI is the GitHub REST API base. Overridable in tests.
const defaultGitHubAPI = "https://api.github.com"

// ghAsset is a single downloadable file attached to a GitHub release.
type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// ghRelease is the subset of the GitHub release payload we consume.
type ghRelease struct {
	TagName string    `json:"tag_name"`
	Name    string    `json:"name"`
	Assets  []ghAsset `json:"assets"`
}

// updater carries everything needed to fetch a release and replace the running
// binary. Fields are injectable so the flow can be exercised without touching
// the network or the real executable.
type updater struct {
	repo           string
	currentVersion string
	goos, goarch   string
	apiBase        string
	httpClient     *http.Client
	out            io.Writer
}

// runUpdate is the `aviary update` subcommand entry point. It parses the
// update-specific flags, builds an updater for the running platform, and
// returns a process exit code.
func runUpdate(args []string, currentVersion string, out io.Writer) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "Usage: aviary update [--check] [--version vX.Y.Z] [--force] [--repo owner/name]")
		fmt.Fprintln(out, "\nDownload the matching GitHub release build and atomically replace this binary.")
		fmt.Fprintln(out, "\nFlags:")
		fs.PrintDefaults()
	}
	check := fs.Bool("check", false, "report the latest version without installing it")
	pin := fs.String("version", "", "install this exact version instead of the latest (e.g. v1.2.3)")
	force := fs.Bool("force", false, "update even from a non-release (untracked) build")
	repo := fs.String("repo", defaultRepo, "GitHub owner/name to fetch releases from")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	u := &updater{
		repo:           *repo,
		currentVersion: currentVersion,
		goos:           runtime.GOOS,
		goarch:         runtime.GOARCH,
		apiBase:        defaultGitHubAPI,
		httpClient:     &http.Client{Timeout: 60 * time.Second},
		out:            out,
	}
	if err := u.run(*pin, *check, *force); err != nil {
		fmt.Fprintf(out, "aviary update: %v\n", err)
		return 1
	}
	return 0
}

// run performs the update (or, when check is true, only reports availability).
func (u *updater) run(pin string, check, force bool) error {
	rel, err := u.fetchRelease(pin)
	if err != nil {
		return err
	}
	latest := normalizeVersion(rel.TagName)
	current := normalizeVersion(u.currentVersion)

	fmt.Fprintf(u.out, "current version: %s\n", displayVersion(u.currentVersion))
	fmt.Fprintf(u.out, "latest version:  %s\n", rel.TagName)

	released := isReleaseBuild(u.currentVersion)
	cmp := compareSemver(current, latest)
	upToDate := released && cmp >= 0

	if check {
		if upToDate {
			fmt.Fprintln(u.out, "you are on the latest version")
		} else {
			fmt.Fprintln(u.out, "an update is available; run `aviary update` to install it")
		}
		return nil
	}

	if !released && !force {
		return errors.New("this is not a release build; re-run with --force to update anyway")
	}
	if upToDate && !force {
		fmt.Fprintln(u.out, "already up to date")
		return nil
	}

	assetName := assetName(latest, u.goos, u.goarch)
	asset, ok := findAsset(rel, assetName)
	if !ok {
		return fmt.Errorf("no release asset %q for %s/%s", assetName, u.goos, u.goarch)
	}
	sums, ok := findAsset(rel, "checksums.txt")
	if !ok {
		return errors.New("release is missing checksums.txt")
	}

	fmt.Fprintf(u.out, "downloading %s ...\n", asset.Name)
	archive, err := u.download(asset.URL)
	if err != nil {
		return fmt.Errorf("download archive: %w", err)
	}
	sumData, err := u.download(sums.URL)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}

	want, ok := parseChecksums(sumData)[asset.Name]
	if !ok {
		return fmt.Errorf("checksums.txt has no entry for %q", asset.Name)
	}
	if err := verifyChecksum(archive, want); err != nil {
		return err
	}

	bin, err := extractBinary(archive, u.goos)
	if err != nil {
		return err
	}

	exe, err := resolveExecutable()
	if err != nil {
		return err
	}
	if err := replaceExecutable(exe, bin, u.goos); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}

	fmt.Fprintf(u.out, "updated %s → %s\n", displayVersion(u.currentVersion), rel.TagName)
	fmt.Fprintln(u.out, "restart any running aviary server to load the new binary")
	return nil
}

// fetchRelease returns the latest release, or the release tagged v<pin> when
// pin is non-empty.
func (u *updater) fetchRelease(pin string) (*ghRelease, error) {
	var endpoint string
	if pin == "" {
		endpoint = fmt.Sprintf("%s/repos/%s/releases/latest", u.apiBase, u.repo)
	} else {
		endpoint = fmt.Sprintf("%s/repos/%s/releases/tags/v%s", u.apiBase, u.repo, normalizeVersion(pin))
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "aviary-selfupdate")
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api %s: %s", endpoint, resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if rel.TagName == "" {
		return nil, errors.New("release has no tag_name")
	}
	return &rel, nil
}

// download fetches the body at url with a size guard.
func (u *updater) download(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "aviary-selfupdate")
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	const maxDownload = 200 << 20 // 200 MiB
	return io.ReadAll(io.LimitReader(resp.Body, maxDownload))
}

// assetName builds the release archive name for a platform, mirroring the
// name_template in .goreleaser.yml: aviary_<version>_<os>_<arch>[v<arm>].zip.
// version must already be normalized (no leading "v").
func assetName(version, goos, goarch string) string {
	arch := goarch
	if goarch == "arm" {
		// GoReleaser builds 32-bit arm with GOARM=7, named "armv7".
		arch = "armv7"
	}
	return fmt.Sprintf("aviary_%s_%s_%s.zip", version, goos, arch)
}

// findAsset locates a release asset by exact name.
func findAsset(rel *ghRelease, name string) (ghAsset, bool) {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return ghAsset{}, false
}

// parseChecksums parses GoReleaser's checksums.txt ("<hex>  <filename>" lines)
// into a filename→hash map.
func parseChecksums(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		out[fields[1]] = strings.ToLower(fields[0])
	}
	return out
}

// verifyChecksum confirms the SHA-256 of data matches want (hex).
func verifyChecksum(data []byte, want string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	return nil
}

// extractBinary reads the aviary executable out of a release zip archive.
func extractBinary(archive []byte, goos string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	want := "aviary"
	if goos == "windows" {
		want = "aviary.exe"
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != want {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		const maxBinary = 250 << 20 // 250 MiB
		bin, err := io.ReadAll(io.LimitReader(rc, maxBinary))
		if err != nil {
			return nil, err
		}
		if len(bin) == 0 {
			return nil, errors.New("extracted binary is empty")
		}
		return bin, nil
	}
	return nil, fmt.Errorf("archive does not contain %q", want)
}

// resolveExecutable returns the absolute, symlink-resolved path of the running
// binary.
func resolveExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// replaceExecutable atomically swaps newBin in for the binary at exePath. The
// new file is written in the same directory (so the final rename is atomic) and
// the previous binary is kept aside for rollback (".bak" on unix, ".old" on
// windows, where a running exe cannot be replaced in place).
func replaceExecutable(exePath string, newBin []byte, goos string) error {
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".aviary-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(newBin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}

	if goos == "windows" {
		old := exePath + ".old"
		os.Remove(old)
		if err := os.Rename(exePath, old); err != nil {
			return err
		}
		if err := os.Rename(tmpName, exePath); err != nil {
			// roll back the move-aside
			os.Rename(old, exePath)
			return err
		}
		cleanup = false
		return nil
	}

	// Unix: keep a backup, then atomically replace.
	bak := exePath + ".bak"
	if err := copyFile(exePath, bak); err != nil {
		return err
	}
	if err := os.Rename(tmpName, exePath); err != nil {
		return err
	}
	cleanup = false
	os.Remove(bak)
	return nil
}

// copyFile copies src to dst, preserving the executable bit.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// isReleaseBuild reports whether v looks like a real release version (i.e. the
// main.version ldflag was set), as opposed to the default untracked build.
func isReleaseBuild(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && v != "(untracked)"
}

// displayVersion renders a version for human output.
func displayVersion(v string) string {
	if !isReleaseBuild(v) {
		return "(untracked)"
	}
	return v
}

// normalizeVersion strips a leading "v" and surrounding space so tag names and
// ldflag versions compare cleanly.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// compareSemver compares two normalized version strings by their dotted numeric
// core, ignoring any pre-release/build suffix. It returns -1, 0, or 1. A
// version with a pre-release suffix sorts below the same core without one.
func compareSemver(a, b string) int {
	ca, pa := splitCore(a)
	cb, pb := splitCore(b)
	for i := 0; i < 3; i++ {
		na, nb := atoiSafe(ca, i), atoiSafe(cb, i)
		if na != nb {
			if na < nb {
				return -1
			}
			return 1
		}
	}
	// Equal cores: a pre-release (non-empty suffix) is older than a final release.
	switch {
	case pa == "" && pb != "":
		return 1
	case pa != "" && pb == "":
		return -1
	default:
		return strings.Compare(pa, pb)
	}
}

// splitCore separates the x.y.z core from any -prerelease/+build suffix.
func splitCore(v string) ([]string, string) {
	suffix := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		suffix = v[i+1:]
		v = v[:i]
	}
	return strings.Split(v, "."), suffix
}

// atoiSafe returns the integer at index i of parts, or 0 if missing/invalid.
func atoiSafe(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return 0
	}
	return n
}
