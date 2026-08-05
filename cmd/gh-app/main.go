package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	appcache "github.com/ChronoAIProject/gh-app/cmd/gh-app/internal/cache"
	"github.com/pelletier/go-toml/v2"
)

type AppConfig struct {
	AppID      int64    `toml:"app_id"`
	PrivateKey string   `toml:"private_key"`
	Owners     []string `toml:"owners,omitempty"`
	Host       string   `toml:"host,omitempty"`
	APIURL     string   `toml:"api_url,omitempty"`
}

type Config struct {
	Apps []AppConfig `toml:"apps"`
}

type Outcome string

const (
	OutcomeOK               Outcome = "ok"
	OutcomeNoMatch          Outcome = "no-match"
	OutcomeNoContext        Outcome = "no-context"
	OutcomeAmbiguous        Outcome = "ambiguous"
	OutcomeConfigError      Outcome = "config-error"
	OutcomeOperationalError Outcome = "operational-error"
	cacheFallbackExitCode           = 75
	// GitHub rejects App JWTs expiring over 10 minutes ahead; backdating iat absorbs clock skew.
	jwtIssuedAtSkewSeconds = 60
	jwtExpiresAfterSeconds = 9 * 60
)

type Target struct{ Host, Owner, Repo string }
type Result struct {
	Outcome        Outcome
	Token          string
	ExpiresAt      time.Time
	AppID          int64
	InstallationID int64
	Err            error
}

type cacheFallbackError struct{ err error }

func (e cacheFallbackError) Error() string { return e.err.Error() }
func (e cacheFallbackError) Unwrap() error { return e.err }

var httpClient = &http.Client{Timeout: 30 * time.Second}
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	err := dispatch(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh-app:", err)
		var fallback cacheFallbackError
		if errors.As(err, &fallback) {
			os.Exit(cacheFallbackExitCode)
		}
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	switch args[0] {
	case "token":
		return cmdToken(args[1:])
	case "credential":
		return cmdCredential(args[1:])
	case "git-install":
		return cmdGitInstall(args[1:])
	case "shell-init":
		return cmdShellInit(args[1:])
	case "status":
		return cmdStatus()
	case "clear":
		return clearCache()
	case "migrate":
		return cmdMigrate()
	case "version":
		return cmdVersion()
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Print(`gh-app - repository-scoped GitHub App credential resolver

Usage:
  gh-app token [--auto|--target owner/repo]
  gh-app credential [get|store|erase]
  gh-app git-install [--global]
  gh-app shell-init [bash|zsh]
  gh-app status
  gh-app clear
  gh-app migrate
  gh-app version
`)
}

func cmdVersion() error {
	fmt.Println(version)
	return nil
}

func configDir() string {
	if v := os.Getenv("GH_APP_CONFIG_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "gh-app")
	}
	if runtime.GOOS == "windows" {
		d, _ := os.UserConfigDir()
		return filepath.Join(d, "gh-app")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gh-app")
}
func configPath() string       { return filepath.Join(configDir(), "config.toml") }
func legacyConfigPath() string { return filepath.Join(configDir(), "config.json") }

func loadConfig() (Config, error) {
	var cfg Config
	b, err := os.ReadFile(configPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, oldErr := os.Stat(legacyConfigPath()); oldErr == nil {
				return cfg, errors.New("legacy config.json found; run 'gh-app migrate'")
			}
		}
		return cfg, fmt.Errorf("read config: %w", err)
	}
	dec := toml.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config.toml: %w", err)
	}
	if len(cfg.Apps) == 0 {
		return cfg, errors.New("config.toml contains no apps")
	}
	for i := range cfg.Apps {
		a := &cfg.Apps[i]
		if a.AppID == 0 || a.PrivateKey == "" {
			return cfg, fmt.Errorf("apps[%d]: app_id and private_key are required", i)
		}
		if a.Host == "" {
			a.Host = "github.com"
		}
		if a.APIURL == "" {
			if a.Host == "github.com" {
				a.APIURL = "https://api.github.com"
			} else {
				a.APIURL = "https://" + a.Host + "/api/v3"
			}
		}
		a.APIURL = strings.TrimRight(a.APIURL, "/")
		a.PrivateKey = expandHome(a.PrivateKey)
	}
	return cfg, nil
}

func resolve(target Target) Result {
	target = normalizeTarget(target)
	if target.Host == "" || target.Owner == "" {
		return Result{Outcome: OutcomeNoContext}
	}
	cfg, err := loadConfig()
	if err != nil {
		return Result{Outcome: OutcomeConfigError, Err: err}
	}
	entry, ok := cacheStore().GetForApps(cacheTarget(target), configuredAppIDs(cfg))
	if ok {
		return Result{Outcome: OutcomeOK, Token: entry.Token, ExpiresAt: entry.ExpiresAt, AppID: entry.AppID, InstallationID: entry.InstallationID}
	}

	preferred, remaining := candidates(cfg, target)
	for _, group := range [][]AppConfig{preferred, remaining} {
		matches := make([]struct {
			app AppConfig
			id  int64
		}, 0)
		for _, app := range group {
			id, found, probeErr := probeInstallation(app, target)
			if probeErr != nil {
				return Result{Outcome: OutcomeOperationalError, Err: probeErr}
			}
			if found {
				matches = append(matches, struct {
					app AppConfig
					id  int64
				}{app, id})
			}
		}
		if len(matches) > 1 {
			ids := make([]string, len(matches))
			for i, m := range matches {
				ids[i] = strconv.FormatInt(m.app.AppID, 10)
			}
			return Result{Outcome: OutcomeAmbiguous, Err: fmt.Errorf("apps %s all reach %s; disambiguate with owners", strings.Join(ids, ", "), formatTarget(target))}
		}
		if len(matches) == 1 {
			m := matches[0]
			token, exp, mintErr := mintToken(m.app, m.id)
			if mintErr != nil {
				return Result{Outcome: OutcomeOperationalError, Err: mintErr}
			}
			entry := appcache.Entry{Host: target.Host, Owner: target.Owner, Repo: target.Repo, AppID: m.app.AppID, InstallationID: m.id, Token: token, ExpiresAt: exp}
			if err := cacheMintedEntry(entry); err != nil {
				return Result{Outcome: OutcomeOperationalError, Err: err}
			}
			return Result{Outcome: OutcomeOK, Token: token, ExpiresAt: exp, AppID: m.app.AppID, InstallationID: m.id}
		}
	}
	return Result{Outcome: OutcomeNoMatch, Err: fmt.Errorf("no configured App established access to %s", formatTarget(target))}
}

// personalFallbackHelper returns the git credential helper that serves the
// user's own credentials, or "" when gh is not installed.
//
// git-install resets the inherited helper chain so gh-app is consulted before
// the unconditional system helper. That reset also removes gh's own entry, and
// without restoring it a repository no App reaches would be left with no
// credential source at all — which contradicts this tool's stated contract of
// App-first with a personal-credential backstop.
func personalFallbackHelper() string {
	path, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	return "!" + path + " auth git-credential"
}

func candidates(cfg Config, target Target) (preferred, remaining []AppConfig) {
	for _, app := range cfg.Apps {
		if !strings.EqualFold(app.Host, target.Host) {
			continue
		}
		owned := false
		for _, owner := range app.Owners {
			if strings.EqualFold(owner, target.Owner) {
				owned = true
				break
			}
		}
		if owned {
			preferred = append(preferred, app)
		} else {
			remaining = append(remaining, app)
		}
	}
	return
}

func probeInstallation(app AppConfig, target Target) (int64, bool, error) {
	jwt, err := makeJWT(app)
	if err != nil {
		return 0, false, err
	}
	path := "/orgs/" + url.PathEscape(target.Owner) + "/installation"
	if target.Repo != "" {
		path = "/repos/" + url.PathEscape(target.Owner) + "/" + url.PathEscape(target.Repo) + "/installation"
	}
	req, err := apiRequest(http.MethodGet, app.APIURL+path, jwt, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("probe app %d: %w", app.AppID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, false, apiError("probe", app.AppID, resp, body)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID == 0 {
		return 0, false, fmt.Errorf("probe app %d returned an invalid installation: %s", app.AppID, strings.TrimSpace(string(body)))
	}
	return out.ID, true, nil
}

func mintToken(app AppConfig, installationID int64) (string, time.Time, error) {
	jwt, err := makeJWT(app)
	if err != nil {
		return "", time.Time{}, err
	}
	u := fmt.Sprintf("%s/app/installations/%d/access_tokens", app.APIURL, installationID)
	req, err := apiRequest(http.MethodPost, u, jwt, bytes.NewBufferString("{}"))
	if err != nil {
		return "", time.Time{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint token for app %d: %w", app.AppID, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", time.Time{}, apiError("mint token", app.AppID, resp, body)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, err
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("GitHub returned an empty token")
	}
	return out.Token, out.ExpiresAt, nil
}

func apiRequest(method, endpoint, jwt string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gh-app")
	return req, nil
}

func apiError(action string, appID int64, resp *http.Response, body []byte) error {
	return fmt.Errorf("%s for app %d: GitHub API %s: %s", action, appID, resp.Status, strings.TrimSpace(string(body)))
}

func cacheStore() *appcache.Store { return appcache.New(configDir()) }

func cacheTarget(target Target) appcache.Target {
	return appcache.Target{Host: target.Host, Owner: target.Owner, Repo: target.Repo}
}

func configuredAppIDs(cfg Config) []int64 {
	ids := make([]int64, len(cfg.Apps))
	for i, app := range cfg.Apps {
		ids[i] = app.AppID
	}
	return ids
}

func cacheMintedEntry(entry appcache.Entry) error {
	err := cacheStore().Put(entry)
	if isCacheLockTimeout(err) {
		return nil
	}
	return err
}

func invalidateTarget(target Target) error {
	return cacheStore().Invalidate(cacheTarget(target))
}

func clearCache() error {
	err := cacheStore().Clear()
	if isCacheLockTimeout(err) {
		return cacheFallbackError{err}
	}
	return err
}

func isCacheLockTimeout(err error) bool {
	var timeout interface{ CacheLockTimeout() bool }
	return errors.As(err, &timeout) && timeout.CacheLockTimeout()
}

func cmdToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	auto := fs.Bool("auto", false, "infer target from the current repository")
	targetFlag := fs.String("target", "", "target owner/repo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *auto && *targetFlag != "" {
		return errors.New("--auto and --target are mutually exclusive")
	}
	var target Target
	var err error
	if override := os.Getenv("GH_APP_TARGET"); override != "" {
		target, err = explicitTarget(override)
	} else if *targetFlag != "" {
		target, err = explicitTarget(*targetFlag)
	} else if *auto {
		target, err = inferTarget()
	} else {
		return errors.New("one of --auto or --target owner/repo is required")
	}
	if err != nil {
		return err
	}
	r := resolve(target)
	if r.Outcome == OutcomeOK {
		fmt.Println(r.Token)
		return nil
	}
	if r.Outcome == OutcomeNoContext || r.Outcome == OutcomeNoMatch {
		return nil
	}
	if r.Err != nil {
		return fmt.Errorf("%s: %w", r.Outcome, r.Err)
	}
	return errors.New(string(r.Outcome))
}

func cmdCredential(args []string) error {
	op := "get"
	if len(args) > 0 {
		op = args[0]
	}
	data, _ := io.ReadAll(os.Stdin)
	vals := parseCredential(data)
	target, ok := credentialTarget(vals)
	if op == "erase" {
		if ok {
			return invalidateTarget(target)
		}
		return nil
	}
	if op != "get" || !ok {
		return nil
	}
	r := resolve(target)
	switch r.Outcome {
	case OutcomeOK:
		fmt.Printf("username=x-access-token\npassword=%s\npassword_expiry_utc=%d\n\n", r.Token, r.ExpiresAt.Unix())
		return nil
	case OutcomeNoMatch, OutcomeNoContext:
		return nil
	default:
		if r.Err != nil {
			return fmt.Errorf("%s: %w", r.Outcome, r.Err)
		}
		return errors.New(string(r.Outcome))
	}
}

func parseCredential(data []byte) map[string]string {
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			vals[k] = v
		}
	}
	return vals
}

func credentialTarget(vals map[string]string) (Target, bool) {
	if vals["protocol"] != "https" || vals["host"] == "" {
		return Target{}, false
	}
	path := strings.Trim(strings.TrimSuffix(vals["path"], ".git"), "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Target{}, false
	}
	return normalizeTarget(Target{vals["host"], parts[0], parts[1]}), true
}

func cmdGitInstall(args []string) error {
	fs := flag.NewFlagSet("git-install", flag.ContinueOnError)
	global := fs.Bool("global", false, "install globally")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	scope := "--local"
	if *global {
		scope = "--global"
	}
	hosts := map[string]bool{}
	for _, app := range cfg.Apps {
		hosts[app.Host] = true
	}
	ordered := make([]string, 0, len(hosts))
	for host := range hosts {
		ordered = append(ordered, host)
	}
	sort.Strings(ordered)
	self := `!"` + exe + `" credential`
	fallback := personalFallbackHelper()
	if *global {
		for _, host := range ordered {
			fmt.Printf("Resulting credential helper chain for https://%s:\n  helper =\n  helper = %s\n", host, self)
			if fallback != "" {
				fmt.Printf("  helper = %s\n", fallback)
			}
		}
	}
	for _, host := range ordered {
		key := "credential.https://" + host + ".helper"
		cmds := [][]string{
			{"config", scope, "--replace-all", key, ""},
			{"config", scope, "--add", key, self},
		}
		if fallback != "" {
			cmds = append(cmds, []string{"config", scope, "--add", key, fallback})
		}
		cmds = append(cmds, []string{"config", scope, "credential.https://" + host + ".useHttpPath", "true"})
		for _, cmd := range cmds {
			c := exec.Command("git", cmd...)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			if err := c.Run(); err != nil {
				return err
			}
		}
	}
	if fallback == "" {
		fmt.Fprintln(os.Stderr, "warning: gh not found on PATH; repositories no App reaches will have no credential helper")
	}
	fmt.Println("Installed Git credential helper for", strings.Join(ordered, ", "))
	return nil
}

func cmdShellInit(args []string) error {
	shell := "bash"
	if len(args) > 1 {
		return errors.New("shell-init accepts at most one shell")
	}
	if len(args) == 1 {
		shell = args[0]
	}
	if shell != "bash" && shell != "zsh" {
		return fmt.Errorf("unsupported shell %q", shell)
	}
	fmt.Print(`gh() {
  if [ "${GH_APP_DISABLE:-}" = "1" ] || [ -n "${GH_TOKEN:-}" ]; then command gh "$@"; return; fi
	local t resolver_status
	t="$(gh-app token --auto)"
	resolver_status=$?
	if [ "$resolver_status" -eq 75 ]; then command gh "$@"; return; fi
	if [ "$resolver_status" -ne 0 ]; then return "$resolver_status"; fi
  if [ -n "$t" ]; then GH_TOKEN="$t" command gh "$@"; else command gh "$@"; fi
}
`)
	return nil
}

func cmdStatus() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	for _, app := range cfg.Apps {
		jwt, err := makeJWT(app)
		if err != nil {
			return err
		}
		req, _ := apiRequest(http.MethodGet, app.APIURL+"/app/installations", jwt, nil)
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return apiError("list installations", app.AppID, resp, body)
		}
		var installations []struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(body, &installations); err != nil {
			return err
		}
		ids := make([]string, len(installations))
		for i, installation := range installations {
			ids[i] = strconv.FormatInt(installation.ID, 10)
		}
		fmt.Printf("App ID: %d\nHost: %s\nKey: %s\nInstallations: %s\n", app.AppID, app.Host, app.PrivateKey, strings.Join(ids, ", "))
	}
	return nil
}

type legacyConfig struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyPath string `json:"private_key_path"`
	Host           string `json:"host"`
	APIURL         string `json:"api_url"`
}

func cmdMigrate() error {
	if _, err := os.Stat(configPath()); err == nil {
		return errors.New("refusing to overwrite existing config.toml")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := os.ReadFile(legacyConfigPath())
	if err != nil {
		return fmt.Errorf("read legacy config.json: %w", err)
	}
	var old legacyConfig
	if err := json.Unmarshal(b, &old); err != nil {
		return err
	}
	if old.AppID == 0 || old.PrivateKeyPath == "" {
		return errors.New("legacy config is missing app_id or private_key_path")
	}
	app := AppConfig{AppID: old.AppID, PrivateKey: old.PrivateKeyPath, Host: old.Host, APIURL: old.APIURL}
	if app.Host == "" {
		app.Host = "github.com"
	}
	if app.APIURL == "" {
		app.APIURL = "https://api.github.com"
	}
	out, err := toml.Marshal(Config{Apps: []AppConfig{app}})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(configPath(), out, 0600); err != nil {
		return err
	}
	return os.Chmod(configDir(), 0700)
}

func explicitTarget(s string) (Target, error) {
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Target{}, fmt.Errorf("target %q must be owner/repo", s)
	}
	return Target{"github.com", parts[0], strings.TrimSuffix(parts[1], ".git")}, nil
}

func inferTarget() (Target, error) {
	remotesOut, err := exec.Command("git", "remote").Output()
	if err != nil {
		return Target{}, nil
	}
	remote := selectRemote(strings.Fields(string(remotesOut)))
	if remote == "" {
		return Target{}, nil
	}
	out, err := exec.Command("git", "remote", "get-url", remote).Output()
	if err != nil {
		return Target{}, nil
	}
	target, err := parseRemote(strings.TrimSpace(string(out)))
	if err != nil || target == (Target{}) || target.Host == "github.com" {
		return target, err
	}
	b, err := os.ReadFile(configPath())
	if err != nil {
		return Target{}, nil
	}
	var cfg struct {
		Apps []struct {
			Host string `toml:"host"`
		} `toml:"apps"`
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Target{}, nil
	}
	for _, app := range cfg.Apps {
		if strings.EqualFold(app.Host, target.Host) {
			return target, nil
		}
	}
	return Target{}, nil
}

func selectRemote(remotes []string) string {
	for _, remote := range remotes {
		if remote == "origin" {
			return remote
		}
	}
	if len(remotes) == 1 {
		return remotes[0]
	}
	return ""
}

func parseRemote(raw string) (Target, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" {
		return Target{}, nil
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 {
		return Target{}, nil
	}
	return normalizeTarget(Target{u.Hostname(), parts[0], parts[1]}), nil
}

func normalizeTarget(t Target) Target {
	t.Host = strings.ToLower(strings.TrimSpace(t.Host))
	t.Owner = strings.ToLower(strings.TrimSpace(t.Owner))
	t.Repo = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(t.Repo), ".git"))
	return t
}

func formatTarget(t Target) string {
	if t.Repo == "" {
		return t.Host + "/" + t.Owner
	}
	return t.Host + "/" + t.Owner + "/" + t.Repo
}

func validatePrivateKeyFile(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat private key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("private key %s is not a regular file (mode %s)", path, info.Mode())
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("private key %s has mode %04o; permissions must not allow group or other access (0600 or stricter); run chmod 600 %s", path, info.Mode().Perm(), path)
	}
	return nil
}

func makeJWT(app AppConfig) (string, error) {
	path := expandHome(app.PrivateKey)
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open private key %s: %w", path, err)
	}
	defer f.Close()
	if err := validatePrivateKeyFile(f, path); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read private key %s: %w", path, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return "", errors.New("invalid PEM private key")
	}
	var key *rsa.PrivateKey
	if k, e := x509.ParsePKCS1PrivateKey(block.Bytes); e == nil {
		key = k
	} else {
		any, e2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if e2 != nil {
			return "", fmt.Errorf("parse private key: %v / %v", e, e2)
		}
		var ok bool
		key, ok = any.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("private key is not RSA")
		}
	}
	now := time.Now().Unix()
	header := b64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := b64([]byte(`{"iat":` + strconv.FormatInt(now-jwtIssuedAtSkewSeconds, 10) + `,"exp":` + strconv.FormatInt(now+jwtExpiresAfterSeconds, 10) + `,"iss":"` + strconv.FormatInt(app.AppID, 10) + `"}`))
	input := header + "." + payload
	h := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return input + "." + b64(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func expandHome(s string) string {
	if strings.HasPrefix(s, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, s[2:])
	}
	return s
}
