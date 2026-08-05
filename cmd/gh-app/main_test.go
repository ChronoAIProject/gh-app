package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	appcache "github.com/ChronoAIProject/gh-app/cmd/gh-app/internal/cache"
	"github.com/pelletier/go-toml/v2"
)

func TestMain(m *testing.M) {
	if os.Getenv("GH_APP_HOLD_CACHE_LOCK") != "" {
		os.Exit(m.Run())
	}
	os.Unsetenv("GH_APP_CONFIG_DIR")
	realDir := configDir()
	snapshot := func() string {
		var b strings.Builder
		for _, name := range []string{"config.json", "token-cache.json", "config.toml", "cache.json", ".cache.lock", ".cache.flock"} {
			content, err := os.ReadFile(filepath.Join(realDir, name))
			if err != nil {
				fmt.Fprintf(&b, "%s=absent;", name)
			} else {
				fmt.Fprintf(&b, "%s=%x;", name, sha256.Sum256(content))
			}
		}
		return b.String()
	}
	before := snapshot()
	code := m.Run()
	if after := snapshot(); after != before {
		fmt.Fprintf(os.Stderr, "FAIL: tests touched real config directory %s\nbefore: %s\nafter: %s\n", realDir, before, after)
		code = 1
	}
	os.Exit(code)
}

func TestHoldCacheLockProcess(t *testing.T) {
	dir := os.Getenv("GH_APP_HOLD_CACHE_LOCK")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".cache.flock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if _, err := fmt.Fprintln(os.Stdout, "locked"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

var keyOnce sync.Once
var testKey *rsa.PrivateKey

func sharedKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		testKey, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
	})
	return testKey
}

func isolatedConfig(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("GH_APP_CONFIG_DIR", filepath.Join(d, "config"))
	return d
}

func TestConfigDirDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix default path assertion")
	}
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("GH_APP_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	want := filepath.Join(home, ".config", "gh-app")
	if got := configDir(); got != want {
		t.Fatalf("configDir() = %q, want %q", got, want)
	}
}

func TestConfigDirPrecedence(t *testing.T) {
	root := t.TempDir()
	override := filepath.Join(root, "override")
	xdg := filepath.Join(root, "xdg")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("GH_APP_CONFIG_DIR", override)

	if got := configDir(); got != override {
		t.Fatalf("configDir() = %q, want override %q", got, override)
	}
	t.Setenv("GH_APP_CONFIG_DIR", "")
	if got, want := configDir(), filepath.Join(xdg, "gh-app"); got != want {
		t.Fatalf("configDir() = %q, want XDG path %q", got, want)
	}
}

func keyFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(sharedKey(t))})
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func saveConfig(t *testing.T, cfg Config) {
	t.Helper()
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func testApp(id int64, key, api string, owners ...string) AppConfig {
	return AppConfig{AppID: id, PrivateKey: key, Host: "github.com", APIURL: api, Owners: owners}
}

type apiServer struct {
	mu    sync.Mutex
	paths []string
	apps  []string
	fn    func(http.ResponseWriter, *http.Request)
	URL   string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func newAPIServer(t *testing.T, fn func(http.ResponseWriter, *http.Request)) *apiServer {
	t.Helper()
	s := &apiServer{fn: fn, URL: "http://github.test"}
	handler := func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.apps = append(s.apps, jwtIssuer(r.Header.Get("Authorization")))
		s.mu.Unlock()
		fn(w, r)
	}
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(r.Method, r.URL.String(), r.Body)
		request.Header = r.Header.Clone()
		handler(recorder, request)
		return recorder.Result(), nil
	}), Timeout: 30 * time.Second}
	return s
}

func jwtIssuer(auth string) string {
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) != 3 {
		return ""
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(b, &claims)
	return fmt.Sprint(claims["iss"])
}

func installation(w http.ResponseWriter, id int64) { fmt.Fprintf(w, `{"id":%d}`, id) }
func token(w http.ResponseWriter, value string, in time.Duration) {
	fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, value, time.Now().Add(in).UTC().Format(time.RFC3339))
}

func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	err := fn()
	os.Stdout = old
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, err
}

func TestVersionPrintsStampedValue(t *testing.T) {
	old := version
	version = "v1.2.3-test"
	t.Cleanup(func() { version = old })
	out, err := capture(t, func() error { return dispatch([]string{"version"}) })
	if err != nil {
		t.Fatal(err)
	}
	if out != "v1.2.3-test\n" {
		t.Fatalf("version output = %q", out)
	}
}

func stdin(t *testing.T, s string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(p, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; _ = f.Close() })
}

func TestRoutingPicksOnlyMatchingApp(t *testing.T) {
	d := isolatedConfig(t)
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			token(w, "right-token", time.Hour)
			return
		}
		if jwtIssuer(r.Header.Get("Authorization")) == "2" {
			installation(w, 22)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL), testApp(2, key, s.URL)}})
	r := resolve(Target{"github.com", "Acme", "Repo"})
	if r.Outcome != OutcomeOK || r.AppID != 2 || r.InstallationID != 22 || r.Token != "right-token" {
		t.Fatalf("result = %+v", r)
	}
}

func TestTwoMatchesAreAmbiguous(t *testing.T) {
	d := isolatedConfig(t)
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) { installation(w, 9) })
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL), testApp(2, key, s.URL)}})
	r := resolve(Target{"github.com", "acme", "repo"})
	if r.Outcome != OutcomeAmbiguous || !strings.Contains(r.Err.Error(), "1, 2") {
		t.Fatalf("result = %+v", r)
	}
}

func TestAll404DiffersFromOperationalError(t *testing.T) {
	d := isolatedConfig(t)
	key := keyFile(t, d, "key.pem")
	s404 := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s404.URL)}})
	if r := resolve(Target{"github.com", "acme", "repo"}); r.Outcome != OutcomeNoMatch || strings.Contains(r.Err.Error(), "does not exist") {
		t.Fatalf("404 result = %+v", r)
	}
	_ = clearCache()
	s500 := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 500) })
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s500.URL)}})
	if r := resolve(Target{"github.com", "acme", "repo"}); r.Outcome != OutcomeOperationalError {
		t.Fatalf("500 result = %+v", r)
	}
}

func TestOwnersOrderBreaksTieButStillProbes(t *testing.T) {
	d := isolatedConfig(t)
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			token(w, "preferred", time.Hour)
			return
		}
		installation(w, 7)
	})
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL), testApp(2, key, s.URL, "acme")}})
	r := resolve(Target{"github.com", "acme", "repo"})
	if r.Outcome != OutcomeOK || r.AppID != 2 {
		t.Fatalf("result = %+v", r)
	}
	if len(s.apps) < 2 || s.apps[0] != "2" {
		t.Fatalf("probe order = %v", s.apps)
	}
}

func TestOwnersNeverGrantWithoutProbe(t *testing.T) {
	d := isolatedConfig(t)
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL, "acme")}})
	if r := resolve(Target{"github.com", "acme", "repo"}); r.Outcome != OutcomeNoMatch {
		t.Fatalf("result = %+v", r)
	}
	if len(s.paths) != 1 {
		t.Fatalf("probes = %v", s.paths)
	}
}

func TestCacheIsRepoKeyed(t *testing.T) {
	d := isolatedConfig(t)
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/a/installation":
			installation(w, 1)
		case "/app/installations/1/access_tokens":
			token(w, "repo-a", time.Hour)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL)}})
	if r := resolve(Target{"github.com", "acme", "a"}); r.Outcome != OutcomeOK {
		t.Fatal(r.Err)
	}
	if r := resolve(Target{"github.com", "acme", "b"}); r.Outcome != OutcomeNoMatch {
		t.Fatalf("repo B reused A: %+v", r)
	}
}

func TestCacheRefreshAndCredentialInvalidation(t *testing.T) {
	d := isolatedConfig(t)
	var mints int
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			installation(w, 3)
			return
		}
		mints++
		token(w, fmt.Sprintf("token-%d", mints), time.Hour)
	})
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL)}})
	target := Target{"github.com", "acme", "repo"}
	if r := resolve(target); r.Token != "token-1" {
		t.Fatalf("first = %+v", r)
	}
	if err := cacheStore().Put(appcache.Entry{Host: target.Host, Owner: target.Owner, Repo: target.Repo, AppID: 1, InstallationID: 3, Token: "token-1", ExpiresAt: time.Now().Add(4 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if r := resolve(target); r.Token != "token-2" {
		t.Fatalf("refresh = %+v", r)
	}
	stdin(t, "protocol=https\nhost=github.com\npath=acme/repo.git\n")
	if _, err := capture(t, func() error { return cmdCredential([]string{"erase"}) }); err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheStore().Get(cacheTarget(target)); ok {
		t.Fatal("credential erase did not invalidate rejected token")
	}
}

func TestResolveReturnsMintedTokenWhenCachePutTimesOut(t *testing.T) {
	d := isolatedConfig(t)
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			installation(w, 3)
			return
		}
		token(w, "minted-despite-cache-timeout", time.Hour)
	})
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL)}})
	lock := holdCacheLock(t, configDir())
	defer releaseCacheLock(lock)

	r := resolve(Target{"github.com", "acme", "repo"})
	if r.Outcome != OutcomeOK || r.Token != "minted-despite-cache-timeout" {
		t.Fatalf("result = %+v", r)
	}
	if _, err := os.Stat(filepath.Join(configDir(), "cache.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache write unexpectedly succeeded: %v", err)
	}
}

func holdCacheLock(t *testing.T, dir string) *os.File {
	t.Helper()
	lock, err := os.OpenFile(filepath.Join(dir, ".cache.flock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	return lock
}

func releaseCacheLock(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func TestSecurityContracts(t *testing.T) {
	d := isolatedConfig(t)
	key := keyFile(t, d, "key.pem")

	t.Run("JWT lifetime and raw URL encoding", func(t *testing.T) {
		jwt, err := makeJWT(AppConfig{AppID: 42, PrivateKey: key})
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 || strings.Contains(jwt, "=") {
			t.Fatalf("invalid compact JWT %q", jwt)
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims struct{ IAT, EXP int64 }
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		if claims.EXP-claims.IAT != 600 {
			t.Fatalf("JWT lifetime = %d seconds", claims.EXP-claims.IAT)
		}
		if got := b64([]byte{0xfb, 0xff}); got != "-_8" {
			t.Fatalf("b64 = %q, want RawURLEncoding output", got)
		}
	})

	t.Run("PKCS8 RSA key", func(t *testing.T) {
		der, err := x509.MarshalPKCS8PrivateKey(sharedKey(t))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(d, "pkcs8.pem")
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := makeJWT(AppConfig{AppID: 42, PrivateKey: path}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("API error status and message", func(t *testing.T) {
		s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
		})
		_, _, err := mintToken(testApp(42, key, s.URL), 7)
		if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") || !strings.Contains(err.Error(), "Bad credentials") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("status excludes installation tokens", func(t *testing.T) {
		s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `[{"id":7,"token":"installation-token-secret"}]`)
		})
		saveConfig(t, Config{Apps: []AppConfig{testApp(42, key, s.URL)}})
		out, err := capture(t, cmdStatus)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "installation-token-secret") {
			t.Fatalf("status leaked token: %q", out)
		}
	})

	t.Run("expandHome only leading slash form", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		if got := expandHome("~/key.pem"); got != filepath.Join(home, "key.pem") {
			t.Fatalf("expanded path = %q", got)
		}
		for _, path := range []string{"~", "~user/key.pem", "/tmp/~/key.pem"} {
			if got := expandHome(path); got != path {
				t.Fatalf("expandHome(%q) = %q", path, got)
			}
		}
	})
}

func TestStrictTOMLRejectsUnknownKeys(t *testing.T) {
	d := isolatedConfig(t)
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		t.Fatal(err)
	}
	b := []byte("[[apps]]\napp_id=1\nprivate_key='" + keyFile(t, d, "key.pem") + "'\nprivat_key='typo'\n")
	if err := os.WriteFile(configPath(), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("error = %v", err)
	}
}

func TestMigrateConvertsJSONAndRefusesClobber(t *testing.T) {
	d := isolatedConfig(t)
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := legacyConfig{AppID: 42, InstallationID: 99, PrivateKeyPath: keyFile(t, d, "key.pem")}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(legacyConfigPath(), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmdMigrate(); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Apps) != 1 || cfg.Apps[0].AppID != 42 {
		t.Fatalf("config = %+v", cfg)
	}
	before, _ := os.ReadFile(configPath())
	if err := cmdMigrate(); err == nil {
		t.Fatal("migrate clobbered TOML")
	}
	after, _ := os.ReadFile(configPath())
	if !bytes.Equal(before, after) {
		t.Fatal("TOML changed")
	}
	if fileMode(t, configPath()) != 0600 || fileMode(t, configDir()) != 0700 {
		t.Fatal("migration permissions are not private")
	}
}

func TestGitInstallLocalResetsHelperChain(t *testing.T) {
	d := isolatedConfig(t)
	repo := filepath.Join(d, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, "https://api.github.com")}})
	if _, err := capture(t, func() error { return cmdGitInstall(nil) }); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "config", "--local", "--get-all", "credential.https://github.com.helper").Output()
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(values) < 2 || values[0] != "" || !strings.Contains(values[1], "gh-app") {
		t.Fatalf("helper chain = %#v", values)
	}
	if v, _ := exec.Command("git", "config", "--local", "--get", "credential.https://github.com.useHttpPath").Output(); strings.TrimSpace(string(v)) != "true" {
		t.Fatal("useHttpPath not set")
	}
}

// gitInstallChain runs git-install in a throwaway repository with PATH
// controlled, and returns the resulting helper chain.
func gitInstallChain(t *testing.T, withGh bool) []string {
	t.Helper()
	d := isolatedConfig(t)
	repo := filepath.Join(d, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	// A PATH containing git but, unless asked for, no gh at all.
	bin := filepath.Join(d, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	if withGh {
		stub := "#!/bin/sh\nexit 0\n"
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)

	old, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, "https://api.github.com")}})
	if _, err := capture(t, func() error { return cmdGitInstall(nil) }); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "config", "--local", "--get-all", "credential.https://github.com.helper").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

func TestGitInstallAppendsPersonalFallbackAfterGhApp(t *testing.T) {
	values := gitInstallChain(t, true)
	// Resetting the inherited chain also drops gh's own helper. Without
	// restoring it, a repository no App reaches would have no credential
	// source, contradicting the App-first-with-personal-backstop contract.
	if len(values) != 3 {
		t.Fatalf("helper chain = %#v, want reset + gh-app + gh fallback", values)
	}
	if values[0] != "" {
		t.Errorf("chain does not start with the reset entry: %#v", values)
	}
	if !strings.Contains(values[1], "gh-app") {
		t.Errorf("gh-app is not consulted first: %q", values[1])
	}
	if !strings.Contains(values[2], "auth git-credential") {
		t.Errorf("personal fallback is not last: %q", values[2])
	}
}

func TestGitInstallWithoutGhInstallsOnlyItself(t *testing.T) {
	values := gitInstallChain(t, false)
	if len(values) != 2 {
		t.Fatalf("helper chain = %#v, want reset + gh-app only", values)
	}
	if values[0] != "" || !strings.Contains(values[1], "gh-app") {
		t.Fatalf("helper chain = %#v", values)
	}
}

func TestCredentialSecurityAndExpiry(t *testing.T) {
	d := isolatedConfig(t)
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			installation(w, 2)
		} else {
			token(w, "secret", time.Hour)
		}
	})
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL)}})
	stdin(t, "protocol=https\nhost=github.com\npath=acme/repo.git\n")
	out, err := capture(t, func() error { return cmdCredential([]string{"get"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "password=secret") || !strings.Contains(out, "password_expiry_utc=") || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("output = %q", out)
	}
}

func TestCredentialWithholdsForOtherHostsAndInvalidPaths(t *testing.T) {
	d := isolatedConfig(t)
	called := 0
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) { called++; installation(w, 2) })
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL)}})
	for _, input := range []string{"protocol=http\nhost=github.com\npath=a/b\n", "protocol=https\nhost=gitlab.com\npath=a/b\n", "protocol=https\nhost=github.com\n", "protocol=ssh\nhost=github.com\npath=a/b\n"} {
		stdin(t, input)
		out, err := capture(t, func() error { return cmdCredential([]string{"get"}) })
		if err != nil {
			t.Fatal(err)
		}
		if out != "" {
			t.Fatalf("leaked for %q: %q", input, out)
		}
	}
	if called != 0 {
		t.Fatalf("API called %d times", called)
	}
}

func TestAutoNonGitHubRemoteDelegatesBeforeConfigError(t *testing.T) {
	d := isolatedConfig(t)
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(), []byte("not valid toml = ["), 0600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(d, "repo")
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://gitlab.com/acme/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v: %s", err, out)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	out, err := capture(t, func() error { return cmdToken([]string{"--auto"}) })
	if err != nil || out != "" {
		t.Fatalf("non-GitHub fallback: out=%q err=%v", out, err)
	}
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{{AppID: 1, PrivateKey: key, Host: "gitlab.com", APIURL: "https://gitlab.com/api/v3"}}})
	target, err := inferTarget()
	if err != nil || target != (Target{"gitlab.com", "acme", "repo"}) {
		t.Fatalf("configured host inference: target=%+v err=%v", target, err)
	}
}

func TestShellFunctionDegenerateCases(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		shellPath, err := exec.LookPath(shell)
		if err != nil {
			t.Logf("skipping %s: %v", shell, err)
			continue
		}
		source, err := capture(t, func() error { return cmdShellInit([]string{shell}) })
		if err != nil {
			t.Fatal(err)
		}
		bin := t.TempDir()
		record := filepath.Join(bin, "gh.record")
		appRecord := filepath.Join(bin, "gh-app.record")
		ghStub := "#!/bin/sh\n{ printf 'token=%s\\n' \"${GH_TOKEN-}\"; for arg do printf 'arg=%s\\n' \"$arg\"; done; } > \"$GH_RECORD\"\n"
		appStub := "#!/bin/sh\nprintf 'called\\n' >> \"$GH_APP_RECORD\"\nif [ -n \"${STUB_ERROR-}\" ]; then printf '%s\\n' \"$STUB_ERROR\" >&2; exit \"${STUB_STATUS:-1}\"; fi\nprintf '%s' \"${STUB_TOKEN-}\"\n"
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(ghStub), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "gh-app"), []byte(appStub), 0700); err != nil {
			t.Fatal(err)
		}

		cases := []struct {
			name, token, disable, preset string
			wantToken                    string
			wantResolver                 bool
			resolverError                string
			resolverStatus               int
			fallback                     bool
		}{
			{"resolved token", "app-token", "", "", "app-token", true, "", 0, false},
			{"no match delegates", "", "", "", "", true, "", 0, false},
			{"disable bypasses", "app-token", "1", "", "", false, "", 0, false},
			{"preset token untouched", "app-token", "", "personal-token", "personal-token", false, "", 0, false},
			{"cache timeout delegates", "", "", "", "", true, "operational-error: timed out waiting for cache lock", cacheFallbackExitCode, true},
			{"ambiguous surfaces error", "", "", "", "", true, "ambiguous: apps 1 and 2 match", 17, false},
			{"config error surfaces error", "", "", "", "", true, "config-error: unknown key", 19, false},
		}
		for _, tc := range cases {
			t.Run(shell+"/"+tc.name, func(t *testing.T) {
				_ = os.Remove(record)
				_ = os.Remove(appRecord)
				cmd := exec.Command(shellPath, "-c", source+"\ngh 'arg one' '--flag=x'")
				env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GH_RECORD=" + record, "GH_APP_RECORD=" + appRecord, "STUB_TOKEN=" + tc.token, "STUB_ERROR=" + tc.resolverError, fmt.Sprintf("STUB_STATUS=%d", tc.resolverStatus)}
				if tc.disable != "" {
					env = append(env, "GH_APP_DISABLE="+tc.disable)
				}
				if tc.preset != "" {
					env = append(env, "GH_TOKEN="+tc.preset)
				}
				cmd.Env = env
				out, runErr := cmd.CombinedOutput()
				if tc.resolverError != "" {
					if tc.fallback {
						if runErr != nil || string(out) != tc.resolverError+"\n" {
							t.Fatalf("cache fallback: err=%v output=%q", runErr, out)
						}
					} else {
						exitErr, ok := runErr.(*exec.ExitError)
						if !ok || exitErr.ExitCode() != tc.resolverStatus {
							t.Fatalf("exit = %v, want %d; output=%q", runErr, tc.resolverStatus, out)
						}
						if string(out) != tc.resolverError+"\n" {
							t.Fatalf("stderr = %q, want %q", out, tc.resolverError+"\n")
						}
						if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("plain gh invoked after resolver failure: %v", err)
						}
						return
					}
				}
				if runErr != nil {
					t.Fatalf("execute shell function: %v: %s", runErr, out)
				}
				got, err := os.ReadFile(record)
				if err != nil {
					t.Fatal(err)
				}
				want := "token=" + tc.wantToken + "\narg=arg one\narg=--flag=x\n"
				if string(got) != want {
					t.Fatalf("gh invocation = %q, want %q", got, want)
				}
				_, err = os.Stat(appRecord)
				if called := err == nil; called != tc.wantResolver {
					t.Fatalf("resolver called = %v, want %v", called, tc.wantResolver)
				}
			})
		}
	}
	if r := resolve(Target{}); r.Outcome != OutcomeNoContext {
		t.Fatalf("empty target = %s", r.Outcome)
	}
	if target, _ := parseRemote("git@github.com:acme/repo.git"); target != (Target{}) {
		t.Fatalf("SSH remote accepted: %+v", target)
	}
	if target, _ := parseRemote("https://github.com/acme/repo.git"); target.Repo != "repo" {
		t.Fatalf("HTTPS remote = %+v", target)
	}
	if got := selectRemote([]string{"upstream", "origin"}); got != "origin" {
		t.Fatalf("selected remote = %q", got)
	}
	if got := selectRemote([]string{"one", "two"}); got != "" {
		t.Fatalf("ambiguous remotes selected %q", got)
	}
	if got := selectRemote([]string{"upstream"}); got != "upstream" {
		t.Fatalf("single remote selected %q", got)
	}
}

func TestRealLockTimeoutExits75AndShellDelegates(t *testing.T) {
	shellPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "config")
	bin := filepath.Join(t.TempDir(), "gh-app-real")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(t.TempDir(), "go-cache"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real command: %v: %s", err, out)
	}

	holder := exec.Command(os.Args[0], "-test.run=^TestHoldCacheLockProcess$")
	holder.Env = append(os.Environ(), "GH_APP_HOLD_CACHE_LOCK="+dir)
	holderIn, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	holderOut, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = holderIn.Close()
		_ = holder.Wait()
	})
	if line, err := bufio.NewReader(holderOut).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("lock holder readiness = %q, %v", line, err)
	}

	direct := exec.Command(bin, "clear")
	direct.Env = append(os.Environ(), "GH_APP_CONFIG_DIR="+dir)
	out, err := direct.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != cacheFallbackExitCode {
		t.Fatalf("real command exit = %v, want %d; output=%q", err, cacheFallbackExitCode, out)
	}
	if !strings.Contains(string(out), "timed out waiting for cache lock") {
		t.Fatalf("real command output = %q", out)
	}

	sourceCmd := exec.Command(bin, "shell-init", "bash")
	source, err := sourceCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	record := filepath.Join(stubDir, "gh.record")
	resolverStub := "#!/bin/sh\nexec \"$REAL_GH_APP\" clear\n"
	ghStub := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$GH_RECORD\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "gh-app"), []byte(resolverStub), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "gh"), []byte(ghStub), 0700); err != nil {
		t.Fatal(err)
	}
	run := exec.Command(shellPath, "-c", string(source)+"\ngh api repos/acme/repo")
	run.Env = []string{
		"PATH=" + stubDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_APP_CONFIG_DIR=" + dir,
		"GH_RECORD=" + record,
		"REAL_GH_APP=" + bin,
	}
	shellOut, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("shell function: %v: %s", err, shellOut)
	}
	if got, err := os.ReadFile(record); err != nil || string(got) != "api repos/acme/repo\n" {
		t.Fatalf("delegated gh invocation = %q, %v", got, err)
	}
}

func TestTargetOverrideWinsAndTokenNoMatchPrintsNothing(t *testing.T) {
	d := isolatedConfig(t)
	s := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{testApp(1, key, s.URL)}})
	t.Setenv("GH_APP_TARGET", "override/repo")
	out, err := capture(t, func() error { return cmdToken([]string{"--target", "ignored/repo"}) })
	if err != nil || out != "" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(s.paths) != 1 || s.paths[0] != "/repos/override/repo/installation" {
		t.Fatalf("paths = %v", s.paths)
	}
}

func TestConfigDefaultsAndStaleJSONMessage(t *testing.T) {
	d := isolatedConfig(t)
	key := keyFile(t, d, "key.pem")
	saveConfig(t, Config{Apps: []AppConfig{{AppID: 1, PrivateKey: key}}})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Apps[0].Host != "github.com" || cfg.Apps[0].APIURL != "https://api.github.com" {
		t.Fatalf("defaults = %+v", cfg.Apps[0])
	}
	if err := os.Remove(configPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyConfigPath(), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("error = %v", err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
