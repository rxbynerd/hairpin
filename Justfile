default: build test

build:
    go build -o hairpin ./cmd/hairpin

test:
    go test ./...

test-race:
    go test -race ./internal/...

lint:
    golangci-lint run ./...

proto:
    buf generate

buf-lint:
    buf lint

# Re-vendor the harness proto from a local stirrup checkout, then regenerate.
sync-proto stirrup_dir="../stirrup":
    cp {{stirrup_dir}}/proto/harness/v1/harness.proto proto/harness/v1/harness.proto
    buf generate

image tag="localhost/hairpin:dev":
    podman build -t {{tag}} -f Containerfile .

# Development cluster: create, deploy, exercise, destroy.
kind-up:
    ./scripts/dev/kind-up.sh

deploy:
    ./scripts/dev/deploy.sh

smoke-test:
    ./scripts/dev/smoke-test.sh

kind-down:
    ./scripts/dev/kind-down.sh

clean:
    rm -f hairpin
