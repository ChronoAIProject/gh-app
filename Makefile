BINARY := gh-app
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST_TARGETS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64
MAIN_COVERAGE_FLOOR := 76.0
CACHE_COVERAGE_FLOOR := 83.0

# Optional local settings for `make test-e2e`, ignored by git. See README.
-include .env
export GH_APP_E2E_APP_ID GH_APP_E2E_KEY GH_APP_E2E_OWNER GH_APP_E2E_REPO
export GH_APP_E2E_HOST GH_APP_E2E_API_URL

.PHONY: build test test-e2e clean dist format-check vet coverage verify
build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/gh-app

format-check:
	@files="$$(gofmt -l .)"; test -z "$$files" || { echo "gofmt required:"; echo "$$files"; exit 1; }

vet:
	go vet ./...

coverage:
	@set -e; \
	main="$$(go test -cover ./cmd/gh-app | sed -n 's/.*coverage: \([0-9.]*\)%.*/\1/p')"; \
	cache="$$(go test -cover ./cmd/gh-app/internal/cache | sed -n 's/.*coverage: \([0-9.]*\)%.*/\1/p')"; \
	test -n "$$main" && test -n "$$cache"; \
	awk -v got="$$main" -v floor="$(MAIN_COVERAGE_FLOOR)" 'BEGIN { if (got + 0 < floor + 0) { printf "cmd/gh-app coverage %s%% is below %s%%\n", got, floor; exit 1 } }'; \
	awk -v got="$$cache" -v floor="$(CACHE_COVERAGE_FLOOR)" 'BEGIN { if (got + 0 < floor + 0) { printf "internal/cache coverage %s%% is below %s%%\n", got, floor; exit 1 } }'; \
	echo "coverage: cmd/gh-app $$main% (minimum $(MAIN_COVERAGE_FLOOR)%); internal/cache $$cache% (minimum $(CACHE_COVERAGE_FLOOR)%)"

verify: format-check vet test coverage build

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
	@set -e; for target in $(DIST_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" -o dist/gh-app-$$os-$$arch ./cmd/gh-app; \
	done
