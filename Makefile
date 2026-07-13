BINARY=colosseum
GO_PACKAGES=./cmd/... ./internal/...

.PHONY: build test test-go test-ui lint lint-go lint-ui check run ui-install ui-build dev tidy clean

build: ui-build
	go build -o bin/$(BINARY) ./cmd/colosseum

test: test-go test-ui

test-go:
	go test $(GO_PACKAGES)

test-ui:
	npm --prefix ui run test

lint: lint-go lint-ui

lint-go:
	go vet $(GO_PACKAGES)

lint-ui:
	npm --prefix ui run lint

check: lint test ui-build

run:
	go run -tags dev ./cmd/colosseum server

ui-install:
	npm --prefix ui install

ui-build:
	npm --prefix ui run build
	rm -rf internal/api/ui/dist
	mkdir -p internal/api/ui/dist
	cp -r ui/dist/. internal/api/ui/dist/

dev:
	( go run -tags dev ./cmd/colosseum server & trap 'kill $$!' INT TERM EXIT; npm --prefix ui run dev )

tidy:
	go mod tidy

clean:
	rm -rf bin artifacts build tmp-artifacts workspaces userfiles
	rm -rf ui/dist internal/api/ui/dist
	rm -f *.db *.db-*
