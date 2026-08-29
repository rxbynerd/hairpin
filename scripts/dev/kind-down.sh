#!/usr/bin/env bash
# Delete the hairpin development cluster and everything in it.

set -euo pipefail

CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"

printf '[kind-down] deleting cluster %s\n' "${CLUSTER_NAME}"
kind delete cluster --name "${CLUSTER_NAME}"
