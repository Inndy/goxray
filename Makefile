.PHONY: all build ui ui-dev test clean

all: build

# The UI bundle is committed under internal/web/dist so `go build` and
# `go install` alone yield a working binary. Rebuild it whenever ui/ changes.
#
# No -o: an explicit one suppresses the platform suffix, so `-o goxray` built to
# a file nobody runs on Windows — make succeeded, goxray.exe kept whatever it
# had, and the stale binary served a stale UI with no failure to point at it.
# Bare `go build` names the output after the package dir, with the suffix.
build: ui
	go build ./cmd/goxray

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
	rm -f goxray goxray.exe
	rm -rf internal/web/dist/assets
