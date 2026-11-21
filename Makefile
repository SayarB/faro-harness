.PHONY: test cli frontend-build dev build clean

test:
	go test -v ./internal/... ./cmd/...

cli:
	go build -o bin/faro-cli ./cmd/faro-cli

frontend-build:
	cd frontend && npm run build

dev:
	wails dev

build:
	wails build

clean:
	rm -rf bin build/bin frontend/dist
