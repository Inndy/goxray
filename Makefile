.PHONY: all build ui ui-dev test clean

all: build

# The UI bundle is committed under internal/web/dist so `go build` and
# `go install` alone yield a working binary. Rebuild it whenever ui/ changes.
build: ui
	go build -o goxray ./cmd/goxray

ui:
	pnpm -C ui install
	pnpm -C ui build

# Frontend iteration: vite on :5173 proxying /api to a dashboard on :5600.
# Run `./goxray --port 5599 --ui-port 5600` alongside this.
ui-dev:
	pnpm -C ui dev

test:
	go test ./...

clean:
	rm -f goxray
	rm -rf internal/web/dist/assets
