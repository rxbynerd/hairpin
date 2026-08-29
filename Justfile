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

# Create the kind development cluster.
kind-up:
    ./scripts/dev/kind-up.sh

# Build hairpin, load it into the cluster, and apply the manifests.
deploy:
    ./scripts/dev/deploy.sh

# Submit one job against the cluster and assert it ran in a sandbox Pod.
smoke-test:
    ./scripts/dev/smoke-test.sh

# Submit two jobs and assert the second recalls what the first saved.
memory-smoke-test:
    ./scripts/dev/memory-smoke-test.sh

# Wire an OpenRouter key (from 1Password) and profile into the cluster.
openrouter op_ref="":
    ./scripts/dev/openrouter.sh {{op_ref}}

# Build+load haybale, deploy an in-cluster gitea, and wire the "git" profile.
haybale:
    ./scripts/dev/haybale.sh

# Submit a job through the "git" profile and assert the push landed in gitea.
git-smoke-test:
    ./scripts/dev/git-smoke-test.sh

# Destroy the kind development cluster.
kind-down:
    ./scripts/dev/kind-down.sh

clean:
    rm -f hairpin
