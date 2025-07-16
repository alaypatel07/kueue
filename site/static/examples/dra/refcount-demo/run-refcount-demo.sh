#!/usr/bin/env bash
# Demonstration script for Kueue DRA reference counting feature.
# 1. Apply setup resources (flavor, DRA config, quota, queues, claim template).
# 2. Create two Jobs using shared ResourceClaim (only first counts toward quota).
# 3. Wait until the user deletes those Jobs.
# 4. Create Job using ResourceClaimTemplate that should be admitted.
# 5. Wait until the user deletes that Job.
# 6. Create Job using ResourceClaimTemplate that should be suspended.
# 7. Wait until the user deletes that Job.
# 8. Exit.

set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

function wait_until_deleted() {
  local name=$1
  echo "Waiting for Job $name to be deleted (Ctrl-C to abort)…"
  while kubectl get job "$name" -n default &>/dev/null; do
    sleep 2
  done
}

echo "Applying GPU quota setup…"
kubectl apply -f "$DIR/../gpu-quota-setup.yaml"

echo "Applying refcount demo setup…"
kubectl apply -f "$DIR/setup.yaml"

echo "Creating first Job using shared ResourceClaim (job1-shared-claim)…"
kubectl apply -f "$DIR/job1-shared-claim.yaml"

echo "Creating second Job using shared ResourceClaim (job2-shared-claim)…"
kubectl apply -f "$DIR/job2-shared-claim.yaml"

echo "Creating Job using ResourceClaimTemplate that should be admitted (job3-template-accepted)…"
kubectl apply -f "$DIR/job3-template-accepted.yaml"

echo "Creating Job using ResourceClaimTemplate that should be suspended (job4-template-suspended)…"
kubectl apply -f "$DIR/job4-template-suspended.yaml"

echo "Demo completed." 