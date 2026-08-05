BINARY := gh-app

.PHONY: build test clean dist
build:
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/gh-app

test:
	go test ./...

clean:
	rm -rf dist $(BINARY)

dist: clean test
	mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-darwin-arm64 ./cmd/gh-app
	GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-darwin-amd64 ./cmd/gh-app
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-linux-amd64 ./cmd/gh-app
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-linux-arm64 ./cmd/gh-app
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/gh-app-windows-amd64.exe ./cmd/gh-app
