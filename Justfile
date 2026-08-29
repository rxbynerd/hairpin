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

clean:
    rm -f hairpin
