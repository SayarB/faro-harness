.PHONY: test cli bindings frontend-test frontend-build dev build clean

test:
	go test -v ./internal/... ./cmd/...

cli:
	go build -o bin/faro-cli ./cmd/faro-cli

# Generates frontend/bindings (gitignored). Needed before `npm run build` / `tsc` outside of wails3 dev/build.
# Requires the wails3 CLI (go install github.com/wailsapp/wails/v3/cmd/wails3@v3.1.0) and an existing
# frontend/dist (any file) because main.go embeds it.
bindings:
	mkdir -p frontend/dist && test -e frontend/dist/index.html || echo '<!doctype html>' > frontend/dist/index.html
	wails3 generate bindings -ts -i

frontend-test:
	cd frontend && npm test

frontend-build:
	cd frontend && npm run build

dev:
	wails3 dev

build:
	wails3 build

clean:
	rm -rf bin frontend/dist
