BIN := bin/lampa-web-builder
IMAGE := ghcr.io/huhen/lampa-web-builder
VERSION ?= dev

.PHONY: build test vet docker-build run e2e clean

build:
	go build -ldflags="-X main.Version=$(VERSION)" -o $(BIN) .

test:
	go test ./...

vet:
	go vet ./...

docker-build:
	docker build -t $(IMAGE):dev .

# Local run against the real upstream; state in ./data (gitignored).
run: build
	DATA_DIR=data ASSETS_DIR=. API_KEY=dev DEFAULT_DOMAIN=localhost POLL_INTERVAL=0 LISTEN=:8080 $(BIN)

# Full build with real npm/gulp against pinned upstream; needs network+node.
e2e:
	go test -tags e2e -v -timeout 20m ./internal/pipeline -run TestFullBuildE2E

clean:
	rm -rf bin data
