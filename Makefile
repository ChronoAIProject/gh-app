BINARY := gh-app

# Optional local settings for `make test-e2e`, ignored by git. See README.
-include .env
export GH_APP_E2E_APP_ID GH_APP_E2E_KEY GH_APP_E2E_OWNER GH_APP_E2E_REPO
export GH_APP_E2E_HOST GH_APP_E2E_API_URL

.PHONY: build test test-e2e clean dist
build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/gh-app

# Offline: generates its own keys and serves the GitHub API from httptest.
# The E2E variables are cleared so a half-filled .env cannot break it.
test:
	GH_APP_E2E_APP_ID= GH_APP_E2E_KEY= GH_APP_E2E_OWNER= GH_APP_E2E_REPO= go test ./...

# Live tests against the real GitHub API. Skipped by `make test`; needs
# GH_APP_E2E_APP_ID, GH_APP_E2E_KEY, GH_APP_E2E_OWNER and GH_APP_E2E_REPO.
test-e2e:
	go test -run E2E -v -count=1 ./...

clean:
	rm -rf dist $(BINARY)

dist: clean test
	mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-darwin-arm64 ./cmd/gh-app
	GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-darwin-amd64 ./cmd/gh-app
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-linux-amd64 ./cmd/gh-app
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-linux-arm64 ./cmd/gh-app
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-windows-amd64.exe ./cmd/gh-app
