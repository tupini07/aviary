package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tupini07/aviary/internal/controlplane"
)

const (
	defaultDeployTimeout = 5 * time.Minute
	maxCLIFileCount      = 5000
	maxCLITotalSize      = 250 << 20
)

type deploySettings struct {
	URL       string
	Project   string
	Directory string
	APIKey    string
	Timeout   time.Duration
}

type deployFile struct {
	path string
	name string
	size int64
	info os.FileInfo
}

type cliDeployResult struct {
	Mode  string `json:"mode"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

type cliAPIError struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// runDeploy packages a static build directory and publishes it through the
// control-plane deployment API.
func runDeploy(args []string, ctx cliContext) int {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(ctx.stderr)
	fs.Usage = func() {
		fmt.Fprintln(ctx.stderr, "Usage: aviary deploy [flags] [directory]")
		fmt.Fprintln(ctx.stderr, "\nPackage and replace a project's static files with a local build directory.")
		fmt.Fprintln(ctx.stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	consoleURL := fs.String("url", "", "Aviary control-plane URL (or AVIARY_URL/project config)")
	projectID := fs.String("project", "", "project id (or AVIARY_PROJECT/project config)")
	directory := fs.String("dir", "", "build output directory (or AVIARY_DIR/project config)")
	apiKey := fs.String("key", "", "project API key (prefer AVIARY_KEY or stored credentials)")
	timeout := fs.Duration("timeout", defaultDeployTimeout, "deployment request timeout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(ctx.stderr, "aviary deploy: expected at most one build directory")
		return 2
	}
	positionalDir := ""
	if fs.NArg() == 1 {
		positionalDir = fs.Arg(0)
	}
	if *timeout <= 0 {
		fmt.Fprintln(ctx.stderr, "aviary deploy: timeout must be positive")
		return 2
	}

	settings, err := resolveDeploySettings(ctx, *consoleURL, *projectID, *directory, positionalDir, *apiKey, *timeout)
	if err != nil {
		fmt.Fprintf(ctx.stderr, "aviary deploy: %v\n", err)
		return 2
	}
	manifest, total, err := buildDeployManifest(settings.Directory)
	if err != nil {
		fmt.Fprintf(ctx.stderr, "aviary deploy: %v\n", err)
		return 1
	}
	result, err := uploadDeployment(ctx, settings, manifest)
	if err != nil {
		fmt.Fprintf(ctx.stderr, "aviary deploy: %v\n", err)
		return 1
	}
	if result.Bytes == 0 {
		result.Bytes = total
	}
	fmt.Fprintf(ctx.stdout, "Deployed %d files (%d bytes) to project %s.\n", result.Files, result.Bytes, settings.Project)
	return 0
}

func resolveDeploySettings(ctx cliContext, urlFlag, projectFlag, dirFlag, positionalDir, keyFlag string, timeout time.Duration) (deploySettings, error) {
	cfg, cfgRoot, err := findProjectConfig(ctx.cwd)
	if err != nil && !errors.Is(err, errProjectConfigNotFound) {
		return deploySettings{}, err
	}
	if errors.Is(err, errProjectConfigNotFound) {
		cfg = projectConfig{}
		cfgRoot = ""
	}

	settings := deploySettings{
		URL:     firstNonEmpty(urlFlag, ctx.getenv("AVIARY_URL"), cfg.URL),
		Project: firstNonEmpty(projectFlag, ctx.getenv("AVIARY_PROJECT"), cfg.Project),
		APIKey:  firstNonEmpty(keyFlag, ctx.getenv("AVIARY_KEY")),
		Timeout: timeout,
	}
	settings.URL, err = normalizeConsoleURL(settings.URL)
	if err != nil {
		return deploySettings{}, err
	}
	if !controlplane.ValidID(settings.Project) {
		return deploySettings{}, errors.New("project is required and must be a valid lower-case Aviary project id")
	}

	rawDir := ""
	configRelative := false
	switch {
	case strings.TrimSpace(positionalDir) != "":
		rawDir = positionalDir
	case strings.TrimSpace(dirFlag) != "":
		rawDir = dirFlag
	case strings.TrimSpace(ctx.getenv("AVIARY_DIR")) != "":
		rawDir = ctx.getenv("AVIARY_DIR")
	case strings.TrimSpace(cfg.Directory) != "":
		rawDir = cfg.Directory
		configRelative = true
	default:
		rawDir = "dist"
	}
	if filepath.IsAbs(rawDir) {
		settings.Directory = filepath.Clean(rawDir)
	} else if configRelative {
		settings.Directory = filepath.Join(cfgRoot, filepath.FromSlash(rawDir))
	} else {
		settings.Directory = filepath.Join(ctx.cwd, filepath.FromSlash(rawDir))
	}
	settings.Directory, err = filepath.Abs(settings.Directory)
	if err != nil {
		return deploySettings{}, fmt.Errorf("resolve build directory: %w", err)
	}

	if settings.APIKey == "" {
		settings.APIKey, err = lookupCredential(ctx, settings.URL, settings.Project)
		if err != nil {
			return deploySettings{}, err
		}
	}
	if !strings.HasPrefix(settings.APIKey, "av_") {
		return deploySettings{}, errors.New("project API key is required (set AVIARY_KEY or save project credentials)")
	}
	return settings, nil
}

func buildDeployManifest(root string) ([]deployFile, int64, error) {
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, 0, fmt.Errorf("build directory %q does not exist", root)
	case err != nil:
		return nil, 0, fmt.Errorf("inspect build directory %q: %w", root, err)
	case !info.IsDir():
		return nil, 0, fmt.Errorf("build path %q is not a directory", root)
	}

	files := make([]deployFile, 0)
	var total int64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, path)
			return fmt.Errorf("build contains unsupported symbolic link %q", filepath.ToSlash(rel))
		}
		entryInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !entryInfo.Mode().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			return fmt.Errorf("build contains unsupported non-regular file %q", filepath.ToSlash(rel))
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, deployFile{
			path: path,
			name: filepath.ToSlash(rel),
			size: entryInfo.Size(),
			info: entryInfo,
		})
		total += entryInfo.Size()
		if len(files) > maxCLIFileCount {
			return fmt.Errorf("build contains more than %d files", maxCLIFileCount)
		}
		if total > maxCLITotalSize {
			return fmt.Errorf("build exceeds the %d MiB uncompressed size limit", maxCLITotalSize>>20)
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("scan build directory: %w", err)
	}
	if len(files) == 0 {
		return nil, 0, errors.New("build directory contains no regular files")
	}
	return files, total, nil
}

func uploadDeployment(ctx cliContext, settings deploySettings, manifest []deployFile) (cliDeployResult, error) {
	endpoint, err := url.JoinPath(settings.URL, "api", "projects", settings.Project, "deploy")
	if err != nil {
		return cliDeployResult{}, fmt.Errorf("build deployment URL: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), settings.Timeout)
	defer cancel()

	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(writeDeployArchive(writer, manifest))
	}()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, reader)
	if err != nil {
		_ = reader.Close()
		return cliDeployResult{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+settings.APIKey)
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("User-Agent", "aviary-deploy")

	client := ctx.httpClient
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return cliDeployResult{}, fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return cliDeployResult{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr cliAPIError
		if json.Unmarshal(body, &apiErr) == nil && strings.TrimSpace(apiErr.Error) != "" {
			return cliDeployResult{}, fmt.Errorf("server returned %s: %s", resp.Status, apiErr.Error)
		}
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = "deployment failed"
		}
		return cliDeployResult{}, fmt.Errorf("server returned %s: %s", resp.Status, message)
	}
	var result cliDeployResult
	if err := json.Unmarshal(body, &result); err != nil {
		return cliDeployResult{}, fmt.Errorf("decode success response: %w", err)
	}
	return result, nil
}

func writeDeployArchive(dst io.Writer, manifest []deployFile) error {
	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)
	closeWithError := func(err error) error {
		_ = tw.Close()
		_ = gz.Close()
		return err
	}

	for _, file := range manifest {
		src, err := os.Open(file.path)
		if err != nil {
			return closeWithError(fmt.Errorf("open %q: %w", file.name, err))
		}
		current, err := src.Stat()
		if err != nil {
			_ = src.Close()
			return closeWithError(fmt.Errorf("inspect %q: %w", file.name, err))
		}
		if !os.SameFile(file.info, current) || current.Size() != file.size ||
			!current.ModTime().Equal(file.info.ModTime()) {
			_ = src.Close()
			return closeWithError(fmt.Errorf("build file %q changed while the deployment was being packaged", file.name))
		}
		header := &tar.Header{
			Name:    file.name,
			Mode:    0o644,
			Size:    file.size,
			ModTime: time.Unix(0, 0).UTC(),
		}
		if err := tw.WriteHeader(header); err != nil {
			_ = src.Close()
			return closeWithError(fmt.Errorf("archive %q: %w", file.name, err))
		}
		n, copyErr := io.CopyN(tw, src, file.size)
		if copyErr != nil {
			_ = src.Close()
			return closeWithError(fmt.Errorf("archive %q after %d bytes: %w", file.name, n, copyErr))
		}
		var extra [1]byte
		extraN, extraErr := src.Read(extra[:])
		closeErr := src.Close()
		if extraN != 0 || !errors.Is(extraErr, io.EOF) {
			return closeWithError(fmt.Errorf("build file %q changed while the deployment was being packaged", file.name))
		}
		if closeErr != nil {
			return closeWithError(fmt.Errorf("close %q: %w", file.name, closeErr))
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return fmt.Errorf("finish tar archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finish gzip archive: %w", err)
	}
	return nil
}
