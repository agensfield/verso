package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/agensfield/verso/internal/buildinfo"
)

const (
	defaultReleasesURL = "https://api.github.com/repos/agensfield/verso/releases?per_page=100"
	maxMetadataSize    = 1 << 20
	maxChecksumsSize   = 1 << 20
	maxArchiveSize     = 100 << 20
	maxBinarySize      = 100 << 20
)

type Status string

const (
	StatusUnknown  Status = "unknown"
	StatusCurrent  Status = "current"
	StatusOutdated Status = "outdated"
	StatusAhead    Status = "ahead"
)

type Options struct {
	Executable     string
	CurrentVersion string
	InstallKind    string
	Client         *http.Client
	// ReleasesURL is an injection seam for tests. Production callers leave it empty.
	ReleasesURL string
}

type Result struct {
	Current      string
	Latest       string
	Status       Status
	InstallKind  string
	Path         string
	ResolvedPath string
	Updated      bool
	Guidance     string
}

type release struct {
	TagName string  `json:"tag_name"`
	Draft   bool    `json:"draft"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type target struct {
	path, realPath, kind, guidance string
	info                           os.FileInfo
}

func Check(ctx context.Context, opts Options) (Result, error) {
	resolved, err := resolveTarget(opts)
	if err != nil {
		return Result{}, err
	}
	latest, _, err := latestRelease(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	current := opts.CurrentVersion
	if current == "" {
		current = buildinfo.Version()
	}
	return resultFor(resolved, current, latest.TagName), nil
}

func Update(ctx context.Context, opts Options) (Result, error) {
	resolved, err := resolveTarget(opts)
	if err != nil {
		return Result{}, err
	}
	current := opts.CurrentVersion
	if current == "" {
		current = buildinfo.Version()
	}
	if resolved.guidance != "" {
		return resultFor(resolved, current, ""), errors.New(resolved.guidance)
	}
	latest, allowedOrigin, err := latestRelease(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	result := resultFor(resolved, current, latest.TagName)
	if result.Status == StatusCurrent {
		return result, nil
	}
	if result.Status == StatusAhead {
		return result, fmt.Errorf("installed version %s is newer than latest release %s", current, latest.TagName)
	}
	if result.Status == StatusUnknown {
		return result, fmt.Errorf("cannot compare installed version %q with release %q", current, latest.TagName)
	}
	if !supportedTarget(runtime.GOOS, runtime.GOARCH) {
		return result, fmt.Errorf("self-update is not supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	version := strings.TrimPrefix(latest.TagName, "v")
	artifactName := fmt.Sprintf("verso_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	artifactURL, checksumURL, err := releaseAssets(latest, artifactName, allowedOrigin)
	if err != nil {
		return result, err
	}
	checksums, err := download(ctx, opts, checksumURL, maxChecksumsSize)
	if err != nil {
		return result, fmt.Errorf("download checksums: %w", err)
	}
	wantDigest, err := checksumFor(checksums, artifactName)
	if err != nil {
		return result, err
	}
	archive, err := download(ctx, opts, artifactURL, maxArchiveSize)
	if err != nil {
		return result, fmt.Errorf("download artifact: %w", err)
	}
	gotDigest := sha256.Sum256(archive)
	if hex.EncodeToString(gotDigest[:]) != wantDigest {
		return result, fmt.Errorf("checksum mismatch for %s", artifactName)
	}
	binary, err := extractBinary(archive)
	if err != nil {
		return result, err
	}
	if err := replace(resolved, binary); err != nil {
		return result, err
	}
	result.Updated = true
	return result, nil
}

func resultFor(t target, current, latest string) Result {
	return Result{Current: current, Latest: latest, Status: compareVersions(current, latest), InstallKind: t.kind, Path: t.path, ResolvedPath: t.realPath, Guidance: t.guidance}
}

func resolveTarget(opts Options) (target, error) {
	path := strings.TrimSpace(opts.Executable)
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return target{}, fmt.Errorf("resolve executable: %w", err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return target{}, fmt.Errorf("resolve executable path: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return target{}, fmt.Errorf("resolve executable symlinks: %w", err)
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return target{}, fmt.Errorf("stat executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return target{}, errors.New("resolved executable is not a regular file")
	}
	kind := opts.InstallKind
	if kind == "" {
		kind = buildinfo.InstallKind()
	}
	t := target{path: abs, realPath: realPath, kind: kind, info: info}
	if isHomebrewPath(realPath) {
		t.kind = "homebrew"
		t.guidance = "installed by Homebrew; use `brew upgrade verso`"
		return t, nil
	}
	if kind != "release" && kind != "go" {
		t.kind = "unknown"
		t.guidance = "cannot safely update an unknown local build; install an official Verso release or use the original install method"
	}
	return t, nil
}

func isHomebrewPath(path string) bool {
	normalized := filepath.ToSlash(path)
	_, tail, ok := strings.Cut(normalized, "/Cellar/verso/")
	if !ok {
		return false
	}
	parts := strings.Split(tail, "/")
	return len(parts) == 3 && parts[0] != "" && parts[1] == "bin" && parts[2] == "verso"
}

func supportedTarget(goos, goarch string) bool {
	return (goos == "darwin" || goos == "linux") && (goarch == "amd64" || goarch == "arm64")
}

func latestRelease(ctx context.Context, opts Options) (release, string, error) {
	releasesURL := opts.ReleasesURL
	if releasesURL == "" {
		releasesURL = defaultReleasesURL
	}
	u, err := url.Parse(releasesURL)
	if err != nil {
		return release{}, "", fmt.Errorf("parse releases URL: %w", err)
	}
	if opts.ReleasesURL == "" && (u.Scheme != "https" || u.Hostname() != "api.github.com") {
		return release{}, "", errors.New("release metadata must use the official GitHub HTTPS endpoint")
	}
	body, err := download(ctx, opts, releasesURL, maxMetadataSize)
	if err != nil {
		return release{}, "", fmt.Errorf("download release metadata: %w", err)
	}
	var releases []release
	if err := json.Unmarshal(body, &releases); err != nil {
		return release{}, "", fmt.Errorf("decode release metadata: %w", err)
	}
	var best release
	var bestVersion version
	found := false
	for _, candidate := range releases {
		parsed, ok := parseVersion(candidate.TagName)
		if candidate.Draft || !ok {
			continue
		}
		if !found || parsed.compare(bestVersion) > 0 {
			best, bestVersion, found = candidate, parsed, true
		}
	}
	if !found {
		return release{}, "", errors.New("no valid Verso releases found")
	}
	return best, u.Scheme + "://" + u.Host, nil
}

func releaseAssets(rel release, artifactName, allowedOrigin string) (string, string, error) {
	var artifactURL, checksumURL string
	for _, asset := range rel.Assets {
		switch asset.Name {
		case artifactName:
			if artifactURL != "" {
				return "", "", fmt.Errorf("duplicate release asset %s", artifactName)
			}
			artifactURL = asset.URL
		case "checksums.txt":
			if checksumURL != "" {
				return "", "", errors.New("duplicate release asset checksums.txt")
			}
			checksumURL = asset.URL
		}
	}
	if artifactURL == "" || checksumURL == "" {
		return "", "", fmt.Errorf("release %s is missing %s or checksums.txt", rel.TagName, artifactName)
	}
	for _, raw := range []string{artifactURL, checksumURL} {
		u, err := url.Parse(raw)
		allowed := err == nil && u.Scheme+"://"+u.Host == allowedOrigin
		if allowedOrigin == "https://api.github.com" {
			allowed = err == nil && u.Scheme == "https" && u.Hostname() == "github.com" && strings.HasPrefix(u.EscapedPath(), "/agensfield/verso/releases/download/")
		}
		if !allowed {
			return "", "", fmt.Errorf("release asset URL %q is outside the release origin", raw)
		}
	}
	return artifactURL, checksumURL, nil
}

func download(ctx context.Context, opts Options, rawURL string, limit int64) ([]byte, error) {
	if err := validateDownloadURL(rawURL, opts); err != nil {
		return nil, err
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	boundedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(boundedCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "verso-updater")
	boundedClient := *client
	previousRedirect := boundedClient.CheckRedirect
	boundedClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := validateDownloadURL(req.URL.String(), opts); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	resp, err := boundedClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	if err := validateDownloadURL(resp.Request.URL.String(), opts); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d-byte limit", limit)
	}
	return body, nil
}

func validateDownloadURL(rawURL string, opts Options) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid download URL %q", rawURL)
	}
	if opts.ReleasesURL != "" {
		base, baseErr := url.Parse(opts.ReleasesURL)
		if baseErr != nil || u.Scheme != base.Scheme || u.Host != base.Host {
			return fmt.Errorf("download URL %q is outside the injected release origin", rawURL)
		}
		return nil
	}
	if u.Scheme != "https" {
		return errors.New("GitHub downloads must use HTTPS")
	}
	switch u.Hostname() {
	case "api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return nil
	default:
		return fmt.Errorf("download URL %q is not an official GitHub origin", rawURL)
	}
}

func checksumFor(data []byte, name string) (string, error) {
	var digest string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			return "", errors.New("invalid checksums.txt format")
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return "", errors.New("invalid checksum digest")
		}
		if fields[1] != name {
			continue
		}
		if digest != "" {
			return "", fmt.Errorf("duplicate checksum for %s", name)
		}
		digest = strings.ToLower(fields[0])
	}
	if digest == "" {
		return "", fmt.Errorf("missing checksum for %s", name)
	}
	return digest, nil
}

func extractBinary(data []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open release archive: %w", err)
	}
	defer gzipReader.Close()
	archive := tar.NewReader(gzipReader)
	header, err := archive.Next()
	if err != nil {
		return nil, fmt.Errorf("read release archive: %w", err)
	}
	if header.Name != "verso" || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > maxBinarySize {
		return nil, errors.New("release archive must contain exactly one regular verso binary")
	}
	binary, err := io.ReadAll(io.LimitReader(archive, maxBinarySize+1))
	if err != nil || int64(len(binary)) != header.Size {
		return nil, errors.New("read complete verso binary from release archive")
	}
	if _, err := archive.Next(); err != io.EOF {
		return nil, errors.New("release archive contains unexpected entries")
	}
	return binary, nil
}

func replace(t target, binary []byte) (retErr error) {
	dir := filepath.Dir(t.realPath)
	tmp, err := os.CreateTemp(dir, ".verso-update-*")
	if err != nil {
		return fmt.Errorf("create update file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(binary); err != nil {
		return fmt.Errorf("write update file: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return fmt.Errorf("make update executable: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync update file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close update file: %w", err)
	}
	currentRealPath, err := filepath.EvalSymlinks(t.path)
	if err != nil || currentRealPath != t.realPath {
		return errors.New("executable target changed during update")
	}
	currentInfo, err := os.Stat(t.realPath)
	if err != nil || !os.SameFile(t.info, currentInfo) {
		return errors.New("executable identity changed during update")
	}
	if err := os.Rename(tmpPath, t.realPath); err != nil {
		return fmt.Errorf("replace executable atomically: %w", err)
	}
	return nil
}
