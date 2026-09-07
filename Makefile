.PHONY: install generate frontend test build test-install

install:
	cd frontend && pnpm install --frozen-lockfile
	cd backend && GOTOOLCHAIN=go1.27.0 go mod download

generate:
	cd backend && GOTOOLCHAIN=go1.27.0 go run -mod=mod github.com/google/wire/cmd/wire ./cmd/server

frontend:
	cd frontend && pnpm run build

test-install:
	bash -n deploy/install.sh
	bash -n deploy/build-release-info.sh
	bash deploy/tests/install-test.sh

test: frontend test-install
	cd frontend && pnpm run lint:check && pnpm run test:run
	cd backend && GOTOOLCHAIN=go1.27.0 go test -race ./... && GOTOOLCHAIN=go1.27.0 go vet ./...

build: frontend
	cd backend && GOTOOLCHAIN=go1.27.0 go build -o bin/sub2api-enhance ./cmd/server
