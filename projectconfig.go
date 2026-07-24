package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tupini07/aviary/internal/controlplane"
	"golang.org/x/term"
)

const projectConfigName = ".aviary.json"

var errProjectConfigNotFound = errors.New("no .aviary.json found")

type cliContext struct {
	cwd           string
	userConfigDir string
	getenv        func(string) string
	stdin         io.Reader
	stdout        io.Writer
	stderr        io.Writer
	httpClient    *http.Client
}

func defaultCLIContext() cliContext {
	cwd, _ := os.Getwd()
	userConfigDir, _ := os.UserConfigDir()
	return cliContext{
		cwd:           cwd,
		userConfigDir: userConfigDir,
		getenv:        os.Getenv,
		stdin:         os.Stdin,
		stdout:        os.Stdout,
		stderr:        os.Stderr,
	}
}

type projectConfig struct {
	URL       string `json:"url"`
	Project   string `json:"project"`
	Directory string `json:"directory"`
}

type storedCredential struct {
	URL     string `json:"url"`
	Project string `json:"project"`
	APIKey  string `json:"apiKey"`
}

type userConfig struct {
	Credentials []storedCredential `json:"credentials"`
}

// runProject dispatches local project configuration commands.
func runProject(args []string, ctx cliContext) int {
	if len(args) == 0 {
		printProjectUsage(ctx.stderr)
		return 2
	}
	switch args[0] {
	case "init":
		return runProjectInit(args[1:], ctx)
	case "credentials":
		return runProjectCredentials(args[1:], ctx)
	case "-h", "--help", "help":
		printProjectUsage(ctx.stdout)
		return 0
	default:
		fmt.Fprintf(ctx.stderr, "aviary project: unknown command %q\n", args[0])
		printProjectUsage(ctx.stderr)
		return 2
	}
}

func printProjectUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  aviary project init --url URL --project ID [--dir dist] [--force]")
	fmt.Fprintln(w, "  aviary project credentials set [--url URL --project ID]")
	fmt.Fprintln(w, "  aviary project credentials remove [--url URL --project ID]")
}

func runProjectInit(args []string, ctx cliContext) int {
	fs := flag.NewFlagSet("project init", flag.ContinueOnError)
	fs.SetOutput(ctx.stderr)
	fs.Usage = func() {
		fmt.Fprintln(ctx.stderr, "Usage: aviary project init --url URL --project ID [--dir dist] [--force]")
		fmt.Fprintln(ctx.stderr, "\nWrite a commit-safe .aviary.json in the current directory.")
		fmt.Fprintln(ctx.stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	consoleURL := fs.String("url", "", "Aviary control-plane URL (or AVIARY_URL)")
	projectID := fs.String("project", "", "project id (or AVIARY_PROJECT)")
	directory := fs.String("dir", "", "build output directory (or AVIARY_DIR; default dist)")
	force := fs.Bool("force", false, "overwrite an existing .aviary.json")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(ctx.stderr, "aviary project init: unexpected positional arguments")
		return 2
	}

	cfg := projectConfig{
		URL:       firstNonEmpty(*consoleURL, ctx.getenv("AVIARY_URL")),
		Project:   firstNonEmpty(*projectID, ctx.getenv("AVIARY_PROJECT")),
		Directory: firstNonEmpty(*directory, ctx.getenv("AVIARY_DIR"), "dist"),
	}
	var err error
	cfg.URL, err = normalizeConsoleURL(cfg.URL)
	if err != nil {
		fmt.Fprintf(ctx.stderr, "aviary project init: %v\n", err)
		return 2
	}
	if !controlplane.ValidID(cfg.Project) {
		fmt.Fprintln(ctx.stderr, "aviary project init: project must be a valid lower-case Aviary project id")
		return 2
	}
	cfg.Directory = strings.TrimSpace(cfg.Directory)
	if cfg.Directory == "" {
		fmt.Fprintln(ctx.stderr, "aviary project init: build directory is required")
		return 2
	}
	cfg.Directory = filepath.ToSlash(filepath.Clean(cfg.Directory))

	if ctx.cwd == "" {
		fmt.Fprintln(ctx.stderr, "aviary project init: cannot determine the current directory")
		return 1
	}
	path := filepath.Join(ctx.cwd, projectConfigName)
	if err := writeProjectConfig(path, cfg, *force); err != nil {
		fmt.Fprintf(ctx.stderr, "aviary project init: %v\n", err)
		return 1
	}
	fmt.Fprintf(ctx.stdout, "Initialized %s for project %s (%s).\n", projectConfigName, cfg.Project, cfg.Directory)
	return 0
}

func writeProjectConfig(path string, cfg projectConfig, force bool) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	flags := os.O_WRONLY | os.O_CREATE
	if force {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists; pass --force to replace it", path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func runProjectCredentials(args []string, ctx cliContext) int {
	if len(args) == 0 {
		fmt.Fprintln(ctx.stderr, "Usage: aviary project credentials <set|remove>")
		return 2
	}
	switch args[0] {
	case "set":
		return runProjectCredentialsSet(args[1:], ctx)
	case "remove":
		return runProjectCredentialsRemove(args[1:], ctx)
	case "-h", "--help", "help":
		fmt.Fprintln(ctx.stdout, "Usage: aviary project credentials <set|remove>")
		return 0
	default:
		fmt.Fprintf(ctx.stderr, "aviary project credentials: unknown command %q\n", args[0])
		return 2
	}
}

func credentialFlags(command string, args []string, ctx cliContext) (string, string, int) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(ctx.stderr)
	consoleURL := fs.String("url", "", "Aviary control-plane URL (or AVIARY_URL/project config)")
	projectID := fs.String("project", "", "project id (or AVIARY_PROJECT/project config)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", "", 0
		}
		return "", "", 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(ctx.stderr, "aviary %s: unexpected positional arguments\n", command)
		return "", "", 2
	}
	resolvedURL, resolvedProject, err := resolveProjectIdentity(ctx, *consoleURL, *projectID)
	if err != nil {
		fmt.Fprintf(ctx.stderr, "aviary %s: %v\n", command, err)
		return "", "", 2
	}
	return resolvedURL, resolvedProject, -1
}

func runProjectCredentialsSet(args []string, ctx cliContext) int {
	consoleURL, projectID, code := credentialFlags("project credentials set", args, ctx)
	if code >= 0 {
		return code
	}
	key, err := readAPIKey(ctx)
	if err != nil {
		fmt.Fprintf(ctx.stderr, "aviary project credentials set: %v\n", err)
		return 1
	}
	if !strings.HasPrefix(key, "av_") {
		fmt.Fprintln(ctx.stderr, "aviary project credentials set: API key must start with av_")
		return 2
	}
	path, err := saveCredential(ctx, storedCredential{URL: consoleURL, Project: projectID, APIKey: key})
	if err != nil {
		fmt.Fprintf(ctx.stderr, "aviary project credentials set: %v\n", err)
		return 1
	}
	fmt.Fprintf(ctx.stdout, "Saved credentials for project %s in %s.\n", projectID, path)
	return 0
}

func runProjectCredentialsRemove(args []string, ctx cliContext) int {
	consoleURL, projectID, code := credentialFlags("project credentials remove", args, ctx)
	if code >= 0 {
		return code
	}
	path, removed, err := removeCredential(ctx, consoleURL, projectID)
	if err != nil {
		fmt.Fprintf(ctx.stderr, "aviary project credentials remove: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(ctx.stdout, "No stored credentials for project %s.\n", projectID)
		return 0
	}
	fmt.Fprintf(ctx.stdout, "Removed credentials for project %s from %s.\n", projectID, path)
	return 0
}

func resolveProjectIdentity(ctx cliContext, urlFlag, projectFlag string) (string, string, error) {
	cfg, _, err := findProjectConfig(ctx.cwd)
	if err != nil && !errors.Is(err, errProjectConfigNotFound) {
		return "", "", err
	}
	consoleURL := firstNonEmpty(urlFlag, ctx.getenv("AVIARY_URL"), cfg.URL)
	projectID := firstNonEmpty(projectFlag, ctx.getenv("AVIARY_PROJECT"), cfg.Project)
	consoleURL, err = normalizeConsoleURL(consoleURL)
	if err != nil {
		return "", "", err
	}
	if !controlplane.ValidID(projectID) {
		return "", "", errors.New("project is required and must be a valid lower-case Aviary project id")
	}
	return consoleURL, projectID, nil
}

func findProjectConfig(start string) (projectConfig, string, error) {
	if strings.TrimSpace(start) == "" {
		return projectConfig{}, "", errProjectConfigNotFound
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return projectConfig{}, "", fmt.Errorf("resolve current directory: %w", err)
	}
	for {
		path := filepath.Join(dir, projectConfigName)
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			var cfg projectConfig
			if err := decodeStrictJSON(data, &cfg); err != nil {
				return projectConfig{}, "", fmt.Errorf("read %s: %w", path, err)
			}
			return cfg, dir, nil
		case !errors.Is(err, os.ErrNotExist):
			return projectConfig{}, "", fmt.Errorf("read %s: %w", path, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return projectConfig{}, "", errProjectConfigNotFound
		}
		dir = parent
	}
}

func normalizeConsoleURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("control-plane URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid control-plane URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("control-plane URL must be an absolute http:// or https:// URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("control-plane URL must contain only scheme and host")
	}
	u.Path = ""
	return strings.TrimRight(u.String(), "/"), nil
}

func readAPIKey(ctx cliContext) (string, error) {
	if key := strings.TrimSpace(ctx.getenv("AVIARY_KEY")); key != "" {
		return key, nil
	}
	if f, ok := ctx.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(ctx.stderr, "Project API key: ")
		data, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(ctx.stderr)
		if err != nil {
			return "", fmt.Errorf("read API key: %w", err)
		}
		key := strings.TrimSpace(string(data))
		if key == "" {
			return "", errors.New("API key is required")
		}
		return key, nil
	}
	scanner := bufio.NewScanner(ctx.stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("read API key: %w", err)
		}
		return "", errors.New("API key is required on stdin or in AVIARY_KEY")
	}
	key := strings.TrimSpace(scanner.Text())
	if key == "" {
		return "", errors.New("API key is required")
	}
	return key, nil
}

func loadUserConfig(ctx cliContext) (userConfig, string, error) {
	path, err := userConfigPath(ctx)
	if err != nil {
		return userConfig{}, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return userConfig{Credentials: make([]storedCredential, 0)}, path, nil
	}
	if err != nil {
		return userConfig{}, path, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg userConfig
	if err := decodeStrictJSON(data, &cfg); err != nil {
		return userConfig{}, path, fmt.Errorf("read %s: %w", path, err)
	}
	if cfg.Credentials == nil {
		cfg.Credentials = make([]storedCredential, 0)
	}
	return cfg, path, nil
}

func lookupCredential(ctx cliContext, consoleURL, projectID string) (string, error) {
	if strings.TrimSpace(ctx.userConfigDir) == "" {
		return "", nil
	}
	cfg, _, err := loadUserConfig(ctx)
	if err != nil {
		return "", err
	}
	for _, cred := range cfg.Credentials {
		if cred.URL == consoleURL && cred.Project == projectID {
			return cred.APIKey, nil
		}
	}
	return "", nil
}

func saveCredential(ctx cliContext, credential storedCredential) (string, error) {
	path, err := userConfigPath(ctx)
	if err != nil {
		return "", err
	}
	unlock, err := lockUserConfig(path)
	if err != nil {
		return path, err
	}
	defer unlock()

	cfg, path, err := loadUserConfig(ctx)
	if err != nil {
		return path, err
	}
	replaced := false
	for i := range cfg.Credentials {
		if cfg.Credentials[i].URL == credential.URL && cfg.Credentials[i].Project == credential.Project {
			cfg.Credentials[i] = credential
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Credentials = append(cfg.Credentials, credential)
	}
	sort.Slice(cfg.Credentials, func(i, j int) bool {
		if cfg.Credentials[i].URL != cfg.Credentials[j].URL {
			return cfg.Credentials[i].URL < cfg.Credentials[j].URL
		}
		return cfg.Credentials[i].Project < cfg.Credentials[j].Project
	})
	return path, writeUserConfig(path, cfg)
}

func removeCredential(ctx cliContext, consoleURL, projectID string) (string, bool, error) {
	path, err := userConfigPath(ctx)
	if err != nil {
		return "", false, err
	}
	unlock, err := lockUserConfig(path)
	if err != nil {
		return path, false, err
	}
	defer unlock()

	cfg, path, err := loadUserConfig(ctx)
	if err != nil {
		return path, false, err
	}
	filtered := cfg.Credentials[:0]
	removed := false
	for _, cred := range cfg.Credentials {
		if cred.URL == consoleURL && cred.Project == projectID {
			removed = true
			continue
		}
		filtered = append(filtered, cred)
	}
	if !removed {
		return path, false, nil
	}
	cfg.Credentials = filtered
	return path, true, writeUserConfig(path, cfg)
}

func writeUserConfig(path string, cfg userConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create user config directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary user config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure temporary user config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary user config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary user config: %w", err)
	}
	if err := replaceFile(tmpPath, path); err != nil {
		return fmt.Errorf("replace user config: %w", err)
	}
	return nil
}

func userConfigPath(ctx cliContext) (string, error) {
	if strings.TrimSpace(ctx.userConfigDir) == "" {
		return "", errors.New("cannot determine the user config directory")
	}
	return filepath.Join(ctx.userConfigDir, "aviary", "config.json"), nil
}

// lockUserConfig guards the credential read-modify-write sequence across
// concurrent CLI processes. A stale lock from a crashed process is reclaimed.
func lockUserConfig(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create user config directory: %w", err)
	}
	lockPath := path + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, writeErr := fmt.Fprintf(lock, "%d\n", os.Getpid())
			closeErr := lock.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(lockPath)
				if writeErr != nil {
					return nil, fmt.Errorf("write user config lock: %w", writeErr)
				}
				return nil, fmt.Errorf("close user config lock: %w", closeErr)
			}
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create user config lock: %w", err)
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 2*time.Minute {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for another Aviary command to update credentials")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func decodeStrictJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
