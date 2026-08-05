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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyPath string `json:"private_key_path"`
	Host           string `json:"host"`
	APIURL         string `json:"api_url"`
}

type Cache struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "token":
		err = cmdToken()
	case "exec":
		err = cmdExec(os.Args[2:])
	case "credential":
		err = cmdCredential(os.Args[2:])
	case "git-install":
		err = cmdGitInstall()
	case "status":
		err = cmdStatus()
	case "clear":
		err = os.Remove(cachePath())
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	case "help", "--help", "-h":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh-app:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`gh-app — use GitHub App credentials with gh and git

Usage:
  gh app init --app-id ID --installation-id ID --key PATH
  gh app token
  gh app exec -- gh pr list
  gh app git-install
  gh app status
  gh app clear

Standalone binary aliases:
  gh-app init ...
  gh-app exec -- gh ...
`)
}

func configDir() string {
	if v := os.Getenv("GH_APP_CONFIG_DIR"); v != "" {
		return v
	}
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "gh-app")
}
func configPath() string { return filepath.Join(configDir(), "config.json") }
func cachePath() string  { return filepath.Join(configDir(), "token-cache.json") }

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	appID := fs.Int64("app-id", 0, "GitHub App ID")
	installID := fs.Int64("installation-id", 0, "GitHub App installation ID")
	key := fs.String("key", "", "path to GitHub App PEM private key")
	host := fs.String("host", "github.com", "GitHub hostname")
	apiURL := fs.String("api-url", "https://api.github.com", "GitHub API base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *appID == 0 || *installID == 0 || *key == "" {
		return errors.New("--app-id, --installation-id and --key are required")
	}
	abs, err := filepath.Abs(expandHome(*key))
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("private key: %w", err)
	}
	cfg := Config{*appID, *installID, abs, *host, strings.TrimRight(*apiURL, "/")}
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(configPath(), b, 0600); err != nil {
		return err
	}
	_ = os.Remove(cachePath())
	fmt.Println("Saved:", configPath())
	return nil
}

func loadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c, fmt.Errorf("read config (run 'gh app init'): %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Host == "" {
		c.Host = "github.com"
	}
	if c.APIURL == "" {
		c.APIURL = "https://api.github.com"
	}
	return c, nil
}

func cmdToken() error {
	t, _, err := getToken()
	if err == nil {
		fmt.Println(t)
	}
	return err
}

func cmdExec(args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("missing command after exec")
	}
	tok, _, err := getToken()
	if err != nil {
		return err
	}
	c := exec.Command(args[0], args[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = append(os.Environ(), "GH_TOKEN="+tok, "GITHUB_TOKEN="+tok)
	return c.Run()
}

func cmdStatus() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	_, exp, err := getToken()
	if err != nil {
		return err
	}
	fmt.Printf("App ID: %d\nInstallation ID: %d\nHost: %s\nKey: %s\nToken expires: %s (%s remaining)\n", cfg.AppID, cfg.InstallationID, cfg.Host, cfg.PrivateKeyPath, exp.Local().Format(time.RFC3339), time.Until(exp).Round(time.Second))
	return nil
}

func cmdGitInstall() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	helper := "!\"" + exe + "\" credential"
	commands := [][]string{
		{"config", "--global", "--replace-all", "credential.https://" + cfg.Host + ".helper", helper},
		{"config", "--global", "credential.https://" + cfg.Host + ".useHttpPath", "true"},
	}
	for _, a := range commands {
		c := exec.Command("git", a...)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return err
		}
	}
	fmt.Println("Installed Git credential helper for https://" + cfg.Host)
	return nil
}

func cmdCredential(args []string) error {
	op := "get"
	if len(args) > 0 {
		op = args[0]
	}
	data, _ := io.ReadAll(os.Stdin)
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			vals[k] = v
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if op == "erase" {
		_ = os.Remove(cachePath())
		return nil
	}
	if op != "get" {
		return nil
	}
	if vals["protocol"] != "https" || vals["host"] != cfg.Host {
		return nil
	}
	tok, exp, err := getToken()
	if err != nil {
		return err
	}
	fmt.Printf("username=x-access-token\npassword=%s\npassword_expiry_utc=%d\n\n", tok, exp.Unix())
	return nil
}

func getToken() (string, time.Time, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", time.Time{}, err
	}
	if b, err := os.ReadFile(cachePath()); err == nil {
		var c Cache
		if json.Unmarshal(b, &c) == nil && c.Token != "" && time.Until(c.ExpiresAt) > 5*time.Minute {
			return c.Token, c.ExpiresAt, nil
		}
	}
	jwt, err := makeJWT(cfg)
	if err != nil {
		return "", time.Time{}, err
	}
	body := bytes.NewBufferString("{}")
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", cfg.APIURL, cfg.InstallationID)
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gh-app")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return "", time.Time{}, fmt.Errorf("GitHub API %s: %s", resp.Status, strings.TrimSpace(string(rb)))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", time.Time{}, err
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("GitHub returned an empty token")
	}
	cache := Cache{out.Token, out.ExpiresAt}
	cb, _ := json.Marshal(cache)
	if err := os.MkdirAll(configDir(), 0700); err == nil {
		_ = os.WriteFile(cachePath(), cb, 0600)
	}
	return out.Token, out.ExpiresAt, nil
}

func makeJWT(cfg Config) (string, error) {
	b, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return "", err
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
	payload := b64([]byte(`{"iat":` + strconv.FormatInt(now-60, 10) + `,"exp":` + strconv.FormatInt(now+540, 10) + `,"iss":"` + strconv.FormatInt(cfg.AppID, 10) + `"}`))
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
