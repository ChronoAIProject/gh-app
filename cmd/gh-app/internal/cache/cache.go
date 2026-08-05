package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const lockTimeout = time.Second

type Target struct {
	Host  string
	Owner string
	Repo  string
}

type Entry struct {
	Host           string    `json:"host"`
	Owner          string    `json:"owner"`
	Repo           string    `json:"repo"`
	AppID          int64     `json:"app_id"`
	InstallationID int64     `json:"installation_id"`
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

type Store struct {
	dir string
}

func New(configDir string) *Store {
	return &Store{dir: configDir}
}

func (s *Store) GetForApps(target Target, configuredAppIDs []int64) (Entry, bool) {
	target = normalizeTarget(target)
	for _, entry := range s.read().Entries {
		if sameTarget(entry, target) && containsAppID(configuredAppIDs, entry.AppID) && entry.Token != "" && time.Until(entry.ExpiresAt) > 5*time.Minute {
			return entry, true
		}
	}
	return Entry{}, false
}

func containsAppID(appIDs []int64, appID int64) bool {
	for _, configured := range appIDs {
		if configured == appID {
			return true
		}
	}
	return false
}

func (s *Store) Put(entry Entry) error {
	target := normalizeTarget(Target{Host: entry.Host, Owner: entry.Owner, Repo: entry.Repo})
	entry.Host, entry.Owner, entry.Repo = target.Host, target.Owner, target.Repo
	return s.transaction(func(snapshot *diskCache) error {
		entries := snapshot.Entries[:0]
		for _, existing := range snapshot.Entries {
			if !sameTarget(existing, target) {
				entries = append(entries, existing)
			}
		}
		snapshot.Entries = append(entries, entry)
		return s.write(snapshot)
	})
}

func (s *Store) Invalidate(target Target) error {
	target = normalizeTarget(target)
	return s.transaction(func(snapshot *diskCache) error {
		entries := snapshot.Entries[:0]
		for _, entry := range snapshot.Entries {
			if !sameTarget(entry, target) {
				entries = append(entries, entry)
			}
		}
		snapshot.Entries = entries
		if len(entries) == 0 {
			err := os.Remove(s.cachePath())
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		return s.write(snapshot)
	})
}

func (s *Store) Clear() error {
	return s.transaction(func(_ *diskCache) error {
		for _, path := range []string{s.cachePath(), filepath.Join(s.dir, ".cache.lock")} {
			err := os.Remove(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	})
}

type diskCache struct {
	Entries []Entry `json:"entries"`
}

type lockTimeoutError struct {
	duration time.Duration
}

func (e lockTimeoutError) Error() string {
	return fmt.Sprintf("timed out waiting for cache lock after %s", e.duration)
}

func (lockTimeoutError) CacheLockTimeout() bool { return true }

func (s *Store) cachePath() string {
	return filepath.Join(s.dir, "cache.json")
}

func (s *Store) read() diskCache {
	var snapshot diskCache
	b, err := os.ReadFile(s.cachePath())
	if err == nil {
		_ = json.Unmarshal(b, &snapshot)
	}
	return snapshot
}

func (s *Store) transaction(fn func(*diskCache) error) error {
	lock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock(lock)
	snapshot := s.read()
	return fn(&snapshot)
}

func (s *Store) lock() (*os.File, error) {
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(s.dir, ".cache.flock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if errors.Is(err, os.ErrExist) {
		lock, err = os.OpenFile(lockPath, os.O_RDWR|syscall.O_NOFOLLOW, 0)
		if err == nil {
			var info os.FileInfo
			info, err = lock.Stat()
			if err == nil && !info.Mode().IsRegular() {
				err = fmt.Errorf("cache lock is not a regular file: %s", lockPath)
			}
			if err == nil {
				err = lock.Chmod(0600)
			}
		}
	}
	if err != nil {
		if lock != nil {
			_ = lock.Close()
		}
		return nil, err
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			_ = lock.Close()
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = lock.Close()
			return nil, lockTimeoutError{duration: lockTimeout}
		}
		if remaining > 10*time.Millisecond {
			remaining = 10 * time.Millisecond
		}
		time.Sleep(remaining)
	}
}

func unlock(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func (s *Store) write(snapshot *diskCache) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".cache-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.cachePath())
}

func (s *Store) ensureDir() error {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	return os.Chmod(s.dir, 0700)
}

func normalizeTarget(target Target) Target {
	target.Host = strings.ToLower(strings.TrimSpace(target.Host))
	target.Owner = strings.ToLower(strings.TrimSpace(target.Owner))
	target.Repo = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(target.Repo), ".git"))
	return target
}

func sameTarget(entry Entry, target Target) bool {
	return strings.EqualFold(entry.Host, target.Host) && strings.EqualFold(entry.Owner, target.Owner) && strings.EqualFold(entry.Repo, target.Repo)
}
