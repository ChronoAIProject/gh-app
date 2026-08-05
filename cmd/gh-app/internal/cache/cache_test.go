package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testEntry(repo string) Entry {
	return Entry{Host: "github.com", Owner: "acme", Repo: repo, AppID: 1, InstallationID: 1, Token: "token-" + repo, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestGetReadsWithoutWaitingForLock(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Put(testEntry("repo")); err != nil {
		t.Fatal(err)
	}
	lock := holdLock(t, dir)
	defer releaseLock(lock)
	entry, ok := store.Get(Target{Host: "GITHUB.COM", Owner: "Acme", Repo: "repo.git"})
	if !ok || entry.Token != "token-repo" {
		t.Fatalf("entry = %+v, found = %v", entry, ok)
	}
}

func TestPutNormalizesEntryForGet(t *testing.T) {
	store := New(t.TempDir())
	entry := testEntry(" Repo.git ")
	entry.Host = " GITHUB.COM "
	entry.Owner = " Acme "
	if err := store.Put(entry); err != nil {
		t.Fatal(err)
	}
	got, ok := store.Get(Target{Host: "github.com", Owner: "acme", Repo: "repo"})
	if !ok || got.Host != "github.com" || got.Owner != "acme" || got.Repo != "repo" {
		t.Fatalf("entry = %+v, found = %v", got, ok)
	}
}

func TestGetRefreshMargin(t *testing.T) {
	store := New(t.TempDir())
	entry := testEntry("repo")
	entry.ExpiresAt = time.Now().Add(5*time.Minute - time.Second)
	if err := store.Put(entry); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(Target{Host: entry.Host, Owner: entry.Owner, Repo: entry.Repo}); ok {
		t.Fatal("entry inside five-minute refresh margin was returned")
	}
}

func TestInvalidatePreservesOtherEntriesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	for _, repo := range []string{"remove", "keep"} {
		if err := store.Put(testEntry(repo)); err != nil {
			t.Fatal(err)
		}
	}
	remove := Target{Host: "GITHUB.COM", Owner: "Acme", Repo: "remove.git"}
	if err := store.Invalidate(remove); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get(remove); ok {
		t.Fatal("invalidated entry remains")
	}
	if entry, ok := store.Get(Target{Host: "github.com", Owner: "acme", Repo: "keep"}); !ok || entry.Token != "token-keep" {
		t.Fatalf("unrelated entry = %+v, found = %v", entry, ok)
	}
	if err := store.Invalidate(Target{Host: "github.com", Owner: "acme", Repo: "keep"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty cache file remains: %v", err)
	}
	if err := store.Invalidate(remove); err != nil {
		t.Fatalf("idempotent invalidation: %v", err)
	}
}

func TestWriterProcess(t *testing.T) {
	dir, repo := os.Getenv("GH_APP_CACHE_TEST_DIR"), os.Getenv("GH_APP_CACHE_TEST_WRITE")
	if dir == "" || repo == "" {
		t.Skip("subprocess helper")
	}
	if err := New(dir).Put(testEntry(repo)); err != nil {
		t.Fatal(err)
	}
}

func TestDeleterProcess(t *testing.T) {
	dir, repo := os.Getenv("GH_APP_CACHE_TEST_DIR"), os.Getenv("GH_APP_CACHE_TEST_DELETE")
	if dir == "" || repo == "" {
		t.Skip("subprocess helper")
	}
	if err := New(dir).Invalidate(Target{Host: "github.com", Owner: "acme", Repo: repo}); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicCacheConcurrentProcesses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	const writers = 20
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			repo := fmt.Sprintf("repo-%d", i)
			cmd := exec.Command(os.Args[0], "-test.run=^TestWriterProcess$")
			cmd.Env = append(os.Environ(), "GH_APP_CACHE_TEST_DIR="+dir, "GH_APP_CACHE_TEST_WRITE="+repo)
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("writer %d: %w: %s", i, err, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	b, err := os.ReadFile(filepath.Join(dir, "cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot diskCache
	if err := json.Unmarshal(b, &snapshot); err != nil {
		t.Fatalf("partial cache: %v", err)
	}
	if len(snapshot.Entries) != writers {
		t.Fatalf("entries = %d, want %d", len(snapshot.Entries), writers)
	}
	seen := make(map[string]bool)
	for _, entry := range snapshot.Entries {
		if !strings.HasPrefix(entry.Repo, "repo-") || entry.Token != "token-"+entry.Repo {
			t.Fatalf("partial or corrupt entry: %+v", entry)
		}
		if seen[entry.Repo] {
			t.Fatalf("duplicate entry for %s", entry.Repo)
		}
		seen[entry.Repo] = true
	}
	if mode := fileMode(t, filepath.Join(dir, "cache.json")); mode != 0600 {
		t.Fatalf("cache mode = %o", mode)
	}
	if mode := fileMode(t, dir); mode != 0700 {
		t.Fatalf("directory mode = %o", mode)
	}
}

func TestPutEnforcesDirectoryAndLockModes(t *testing.T) {
	oldUmask := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	t.Run("new files", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "config")
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := New(dir).Put(testEntry("repo")); err != nil {
			t.Fatal(err)
		}
		assertPrivateCacheModes(t, dir)
	})

	t.Run("existing lock", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "config")
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".cache.flock"), nil, 0644); err != nil {
			t.Fatal(err)
		}
		if err := New(dir).Put(testEntry("repo")); err != nil {
			t.Fatal(err)
		}
		assertPrivateCacheModes(t, dir)
	})
}

func TestConcurrentInsertionNeverResurrectsDeletion(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	store := New(dir)
	const rounds = 20
	for i := 0; i < rounds; i++ {
		rejected := fmt.Sprintf("rejected-%d", i)
		if err := store.Put(testEntry(rejected)); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		run := func(testName, env string) {
			<-start
			cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
			cmd.Env = append(os.Environ(), "GH_APP_CACHE_TEST_DIR="+dir, env)
			out, err := cmd.CombinedOutput()
			if err != nil {
				err = fmt.Errorf("%s: %w: %s", testName, err, out)
			}
			errs <- err
		}
		go run("TestDeleterProcess", "GH_APP_CACHE_TEST_DELETE="+rejected)
		go run("TestWriterProcess", fmt.Sprintf("GH_APP_CACHE_TEST_WRITE=inserted-%d", i))
		close(start)
		for j := 0; j < 2; j++ {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}
		if _, ok := store.Get(Target{Host: "github.com", Owner: "acme", Repo: rejected}); ok {
			t.Fatalf("round %d: concurrent insertion resurrected rejected token", i)
		}
	}
}

func TestClearRacingWriterCannotRestoreCredentials(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Put(testEntry("old")); err != nil {
		t.Fatal(err)
	}
	writerRead := make(chan struct{})
	finishWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- store.transaction(func(snapshot *diskCache) error {
			close(writerRead)
			<-finishWriter
			snapshot.Entries = append(snapshot.Entries, testEntry("new"))
			return store.write(snapshot)
		})
	}()
	<-writerRead
	clearDone := make(chan error, 1)
	clearStarted := make(chan struct{})
	go func() {
		close(clearStarted)
		clearDone <- store.Clear()
	}()
	<-clearStarted
	select {
	case err := <-clearDone:
		t.Fatalf("clear bypassed active writer transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finishWriter)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-clearDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writer restored cache after clear: %v", err)
	}
}

func TestHeldLockBoundsWrites(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	lock := holdLock(t, dir)
	defer releaseLock(lock)
	for name, fn := range map[string]func() error{
		"put":        func() error { return store.Put(testEntry("repo")) },
		"invalidate": func() error { return store.Invalidate(Target{Host: "github.com", Owner: "acme", Repo: "repo"}) },
		"clear":      store.Clear,
	} {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			err := fn()
			var timeout interface{ CacheLockTimeout() bool }
			if !errors.As(err, &timeout) || !timeout.CacheLockTimeout() {
				t.Fatalf("error = %v, want lock timeout", err)
			}
			if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
				t.Fatalf("elapsed = %s", elapsed)
			}
		})
	}
}

func TestClearRemovesLegacyLockDirectory(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".cache.lock")
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	store := New(dir)
	if err := store.Put(testEntry("repo")); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy lock remains: %v", err)
	}
}

func holdLock(t *testing.T, dir string) *os.File {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
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

func releaseLock(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func assertPrivateCacheModes(t *testing.T, dir string) {
	t.Helper()
	for path, want := range map[string]os.FileMode{
		dir:                                0700,
		filepath.Join(dir, ".cache.flock"): 0600,
		filepath.Join(dir, "cache.json"):   0600,
	} {
		if got := fileMode(t, path); got != want {
			t.Errorf("mode for %s = %o, want %o", filepath.Base(path), got, want)
		}
	}
}
