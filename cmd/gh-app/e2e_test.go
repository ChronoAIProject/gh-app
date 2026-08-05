package main

// Live tests are skipped unless all GH_APP_E2E_* values are supplied. Each
// test still routes configuration and cache files through isolatedConfig.

import (
	"os"
	"strconv"
	"testing"
	"time"
)

const (
	envAppID  = "GH_APP_E2E_APP_ID"
	envKey    = "GH_APP_E2E_KEY"
	envOwner  = "GH_APP_E2E_OWNER"
	envRepo   = "GH_APP_E2E_REPO"
	envHost   = "GH_APP_E2E_HOST"
	envAPIURL = "GH_APP_E2E_API_URL"
)

func liveSetup(t *testing.T) Target {
	t.Helper()
	values := []string{os.Getenv(envAppID), os.Getenv(envKey), os.Getenv(envOwner), os.Getenv(envRepo)}
	set := 0
	for _, value := range values {
		if value != "" {
			set++
		}
	}
	if set == 0 {
		t.Skip("live GitHub test; set GH_APP_E2E_APP_ID, GH_APP_E2E_KEY, GH_APP_E2E_OWNER and GH_APP_E2E_REPO")
	}
	if set != len(values) {
		t.Fatal("incomplete GH_APP_E2E_* setup")
	}
	id, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	host, api := os.Getenv(envHost), os.Getenv(envAPIURL)
	if host == "" {
		host = "github.com"
	}
	if api == "" {
		api = "https://api.github.com"
	}
	isolate := isolatedConfig(t)
	_ = isolate
	saveConfig(t, Config{Apps: []AppConfig{{AppID: id, PrivateKey: expandHome(values[1]), Host: host, APIURL: api}}})
	return Target{host, values[2], values[3]}
}

func TestE2EResolvesRepositoryAndIssuesToken(t *testing.T) {
	target := liveSetup(t)
	r := resolve(target)
	if r.Outcome != OutcomeOK {
		t.Fatalf("resolve: %s: %v", r.Outcome, r.Err)
	}
	if r.Token == "" || time.Until(r.ExpiresAt) < 30*time.Minute {
		t.Fatal("invalid installation token or expiry")
	}
}

func TestE2ESecondResolveUsesCache(t *testing.T) {
	target := liveSetup(t)
	first := resolve(target)
	if first.Outcome != OutcomeOK {
		t.Fatal(first.Err)
	}
	second := resolve(target)
	if second.Outcome != OutcomeOK {
		t.Fatal(second.Err)
	}
	if first.Token != second.Token || !first.ExpiresAt.Equal(second.ExpiresAt) {
		t.Fatal("second resolve did not reuse cache")
	}
}

func TestE2EStatusListsInstallations(t *testing.T) {
	liveSetup(t)
	out, err := capture(t, cmdStatus)
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("empty status")
	}
}

func TestE2ECredentialReturnsExpiringPassword(t *testing.T) {
	target := liveSetup(t)
	stdin(t, "protocol=https\nhost="+target.Host+"\npath="+target.Owner+"/"+target.Repo+".git\n")
	out, err := capture(t, func() error { return cmdCredential([]string{"get"}) })
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("credential helper returned no credential")
	}
}
