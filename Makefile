.PHONY: test cli bindings frontend-test frontend-build dev build clean

test:
	go test -v ./internal/... ./cmd/...

cli:
	go build -o bin/faro-cli ./cmd/faro-cli

# Generates frontend/wailsjs (gitignored). Needed before `npm run build` / `tsc` outside of wails dev/build.
# Requires the wails CLI and an existing frontend/dist (any file) because main.go embeds it.
bindings:
	mkdir -p frontend/dist && test -e frontend/dist/index.html || echo '<!doctype html>' > frontend/dist/index.html
	wails generate module

frontend-test:
	cd frontend && npm test

frontend-build:
	cd frontend && npm run build

dev:
	wails dev

build:
	wails build

clean:
	rm -rf bin build/bin frontend/dist
