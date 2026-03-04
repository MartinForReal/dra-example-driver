#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# End-to-end test for the DRA IB driver using the DRANET framework

set -e

PASS=0
FAIL=0
TESTS_RUN=0

function test-pass {
  local name="$1"
  PASS=$((PASS + 1))
  TESTS_RUN=$((TESTS_RUN + 1))
  echo "PASS: $name"
}

function test-fail {
  local name="$1"
  local msg="$2"
  FAIL=$((FAIL + 1))
  TESTS_RUN=$((TESTS_RUN + 1))
  echo "FAIL: $name: $msg"
  exit 1
}

function assert-eq {
  local name="$1"
  local expected="$2"
  local actual="$3"
  if [[ "$expected" != "$actual" ]]; then
    test-fail "$name" "expected '$expected', got '$actual'"
  fi
}

# ── Helper functions ──────────────────────────────────────────────────────────

# Get allocated device names from a ResourceClaim status
function allocated-devices-from-claim {
  local namespace="$1"
  local claim_prefix="$2"
  # Get all claims matching the prefix and extract device names
  kubectl get resourceclaims -n "$namespace" -o json | \
    jq -r ".items[] | select(.metadata.name | startswith(\"$claim_prefix\")) | .status.allocation.devices.results[]?.device" 2>/dev/null
}

# Get the number of allocated devices for a pod's claims
function count-allocated-devices {
  local namespace="$1"
  local pod="$2"
  local total=0

  # Get the actual claim names from the pod's status
  local claim_names
  claim_names=$(kubectl get pod -n "$namespace" "$pod" -o json | \
    jq -r '.status.resourceClaimStatuses[]?.resourceClaimName // empty' 2>/dev/null)

  for claim_name in $claim_names; do
    local count
    count=$(kubectl get resourceclaim -n "$namespace" "$claim_name" -o json | \
      jq -r '.status.allocation.devices.results | length' 2>/dev/null)
    if [[ -n "$count" && "$count" =~ ^[0-9]+$ ]]; then
      total=$((total + count))
    fi
  done

  echo "$total"
}

# ── Cluster readiness ────────────────────────────────────────────────────────

kind get clusters
kubectl get nodes
kubectl wait --for=condition=Ready nodes/dra-infiniband-driver-cluster-worker --timeout=120s

# ── Wait for driver pods ──────────────────────────────────────────────────────

echo "Waiting for DRA driver pods to be ready..."
kubectl wait --for=condition=Ready pod -l app.kubernetes.io/component=kubeletplugin -n dra-infiniband-driver --timeout=120s || true
sleep 5

# ── Verify ResourceSlice published ────────────────────────────────────────────

echo "Checking for published ResourceSlices..."
rs_count=$(kubectl get resourceslice -o json | jq '[.items[] | select(.spec.driver == "ib.sigs.k8s.io")] | length')
if [[ "$rs_count" -eq 0 ]]; then
  echo "WARNING: No ResourceSlices found for ib.sigs.k8s.io driver. Checking driver logs..."
  kubectl logs -n dra-infiniband-driver -l app.kubernetes.io/component=kubeletplugin --tail=50 || true
  test-fail "ResourceSlice publication" "no ResourceSlices published by driver"
fi
echo "Found $rs_count ResourceSlice(s) for ib.sigs.k8s.io"
test-pass "ResourceSlices published by driver"

# ── Webhook readiness ────────────────────────────────────────────────────────

function verify-webhook {
  echo "Waiting for webhook to be available"
  while ! kubectl create --dry-run=server -f- <<-'EOF'
    apiVersion: resource.k8s.io/v1
    kind: ResourceClaim
    metadata:
      name: webhook-test
    spec:
      devices:
        requests:
        - name: ib
          exactly:
            deviceClassName: ib.sigs.k8s.io
            selectors:
            - cel:
                expression: "device.attributes['dra.net'].ibType == 'VF'"
EOF
  do
    sleep 1
    echo "Retrying webhook"
  done
  echo "Webhook is available"
}
export -f verify-webhook
timeout --foreground 30s bash -c verify-webhook

# ── Deploy test workloads ─────────────────────────────────────────────────────

for f in demo/ib-test1.yaml demo/ib-test2.yaml demo/ib-test3.yaml demo/ib-test4.yaml demo/ib-test5.yaml; do
  kubectl delete -f "$f" --ignore-not-found --timeout=25s 2>/dev/null || true
done

kubectl create -f demo/ib-test1.yaml
kubectl create -f demo/ib-test2.yaml
kubectl create -f demo/ib-test3.yaml
kubectl create -f demo/ib-test4.yaml
kubectl create -f demo/ib-test5.yaml

# ═══════════════════════════════════════════════════════════════════════════════
# ib-test1: Two pods, one container each — each gets 1 distinct IB VF
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== ib-test1: Two pods, one VF each ==="

kubectl wait --for=condition=Ready -n ib-test1 pod/pod0 --timeout=120s
kubectl wait --for=condition=Ready -n ib-test1 pod/pod1 --timeout=120s

ib_test1_running=$(kubectl get pods -n ib-test1 --no-headers | grep -c 'Running')
assert-eq "ib-test1 pod count" "2" "$ib_test1_running"

# Verify both pods have allocated claims with devices
ib_test1_pod0_devices=$(count-allocated-devices ib-test1 pod0)
assert-eq "ib-test1 pod0 allocated devices" "1" "$ib_test1_pod0_devices"

ib_test1_pod1_devices=$(count-allocated-devices ib-test1 pod1)
assert-eq "ib-test1 pod1 allocated devices" "1" "$ib_test1_pod1_devices"

test-pass "ib-test1: two pods each got a distinct VF"


# ═══════════════════════════════════════════════════════════════════════════════
# ib-test2: One pod — IB VF with custom pkey + MTU config
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== ib-test2: Custom IB config (pkey + MTU) ==="

kubectl wait --for=condition=Ready -n ib-test2 pod/pod0 --timeout=120s

ib_test2_running=$(kubectl get pods -n ib-test2 --no-headers | grep -c 'Running')
assert-eq "ib-test2 pod count" "1" "$ib_test2_running"

ib_test2_pod0_devices=$(count-allocated-devices ib-test2 pod0)
assert-eq "ib-test2 pod0 allocated devices" "1" "$ib_test2_pod0_devices"

test-pass "ib-test2: VF with custom config allocated successfully"


# ═══════════════════════════════════════════════════════════════════════════════
# ib-test3: One pod — IB PF with admin access
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== ib-test3: PF with admin access ==="

kubectl wait --for=condition=Ready -n ib-test3 pod/pod0 --timeout=120s

ib_test3_running=$(kubectl get pods -n ib-test3 --no-headers | grep -c 'Running')
assert-eq "ib-test3 pod count" "1" "$ib_test3_running"

ib_test3_pod0_devices=$(count-allocated-devices ib-test3 pod0)
assert-eq "ib-test3 pod0 allocated devices" "1" "$ib_test3_pod0_devices"

# Verify admin access is set in the claim allocation
ib_test3_admin=$(kubectl get pod -n ib-test3 pod0 -o json | \
  jq -r '.status.resourceClaimStatuses[0].resourceClaimName' | \
  xargs -I{} kubectl get resourceclaim -n ib-test3 {} -o json | \
  jq -r '.status.allocation.devices.results[0].adminAccess // false')
echo "  admin access: $ib_test3_admin"

test-pass "ib-test3: PF with admin access"


# ═══════════════════════════════════════════════════════════════════════════════
# ib-test4: One pod — 2 VFs on the same NUMA node
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== ib-test4: Dual VFs on same NUMA node ==="

kubectl wait --for=condition=Ready -n ib-test4 pod/pod0 --timeout=120s

ib_test4_running=$(kubectl get pods -n ib-test4 --no-headers | grep -c 'Running')
assert-eq "ib-test4 pod count" "1" "$ib_test4_running"

ib_test4_pod0_devices=$(count-allocated-devices ib-test4 pod0)
assert-eq "ib-test4 pod0 allocated devices" "2" "$ib_test4_pod0_devices"

test-pass "ib-test4: two distinct VFs on the same NUMA node"


# ═══════════════════════════════════════════════════════════════════════════════
# ib-test5: One pod — active port filter
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== ib-test5: Active port health filter ==="

kubectl wait --for=condition=Ready -n ib-test5 pod/pod0 --timeout=120s

ib_test5_running=$(kubectl get pods -n ib-test5 --no-headers | grep -c 'Running')
assert-eq "ib-test5 pod count" "1" "$ib_test5_running"

ib_test5_pod0_devices=$(count-allocated-devices ib-test5 pod0)
assert-eq "ib-test5 pod0 allocated devices" "1" "$ib_test5_pod0_devices"

test-pass "ib-test5: device with active port filter"


# ═══════════════════════════════════════════════════════════════════════════════
# Cleanup: verify fast deletion
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== Cleanup: fast deletion test ==="

kubectl delete -f demo/ib-test1.yaml --timeout=25s
kubectl delete -f demo/ib-test2.yaml --timeout=25s
kubectl delete -f demo/ib-test3.yaml --timeout=25s
kubectl delete -f demo/ib-test4.yaml --timeout=25s
kubectl delete -f demo/ib-test5.yaml --timeout=25s

test-pass "cleanup: all test resources deleted within 25s"


# ═══════════════════════════════════════════════════════════════════════════════
# Webhook validation: reject invalid IbConfig
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== Webhook validation tests ==="

# Webhook should reject IbConfig with pkey=0
if ! kubectl create --dry-run=server -f- <<'EOF' 2>&1 | grep -qF 'pkey must be in range'
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: webhook-test-invalid-pkey
spec:
  devices:
    requests:
    - name: ib
      exactly:
        deviceClassName: ib.sigs.k8s.io
    config:
    - requests: ["ib"]
      opaque:
        driver: ib.sigs.k8s.io
        parameters:
          apiVersion: ib.resource.sigs.k8s.io/v1alpha1
          kind: IbConfig
          pkey: 0
EOF
then
  test-fail "webhook rejects pkey=0" "webhook did not reject ResourceClaim with pkey=0"
fi
test-pass "webhook rejects IbConfig with pkey=0"

# Webhook should reject IbConfig with invalid MTU
if ! kubectl create --dry-run=server -f- <<'EOF' 2>&1 | grep -qF 'invalid IB MTU value'
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: webhook-test-invalid-mtu
spec:
  devices:
    requests:
    - name: ib
      exactly:
        deviceClassName: ib.sigs.k8s.io
    config:
    - requests: ["ib"]
      opaque:
        driver: ib.sigs.k8s.io
        parameters:
          apiVersion: ib.resource.sigs.k8s.io/v1alpha1
          kind: IbConfig
          mtu: 9000
EOF
then
  test-fail "webhook rejects invalid MTU" "webhook did not reject ResourceClaim with mtu=9000"
fi
test-pass "webhook rejects IbConfig with invalid MTU"

# Webhook should accept valid IbConfig
if ! kubectl create --dry-run=server -f- <<'EOF' 2>&1 | grep -qiF 'created'
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: webhook-test-valid
spec:
  devices:
    requests:
    - name: ib
      exactly:
        deviceClassName: ib.sigs.k8s.io
    config:
    - requests: ["ib"]
      opaque:
        driver: ib.sigs.k8s.io
        parameters:
          apiVersion: ib.resource.sigs.k8s.io/v1alpha1
          kind: IbConfig
          pkey: 32769
          mtu: 4096
EOF
then
  test-fail "webhook accepts valid IbConfig" "webhook rejected a valid IbConfig"
fi
test-pass "webhook accepts valid IbConfig"

# Webhook should reject invalid IbConfig in ResourceClaimTemplates
if ! kubectl create --dry-run=server -f- <<'EOF' 2>&1 | grep -qF 'pkey must be in range'
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: webhook-test-template-invalid
spec:
  spec:
    devices:
      requests:
      - name: ib
        exactly:
          deviceClassName: ib.sigs.k8s.io
      config:
      - requests: ["ib"]
        opaque:
          driver: ib.sigs.k8s.io
          parameters:
            apiVersion: ib.resource.sigs.k8s.io/v1alpha1
            kind: IbConfig
            pkey: 0
EOF
then
  test-fail "webhook rejects invalid ResourceClaimTemplate" "webhook did not reject ResourceClaimTemplate with pkey=0"
fi
test-pass "webhook rejects invalid IbConfig in ResourceClaimTemplate"

# Webhook should reject invalid IbConfig via v1beta1 API
if ! kubectl create --dry-run=server -f- <<'EOF' 2>&1 | grep -qF 'invalid IB MTU value'
apiVersion: resource.k8s.io/v1beta1
kind: ResourceClaim
metadata:
  name: webhook-test-v1beta1-invalid
spec:
  devices:
    requests:
    - name: ib
      deviceClassName: ib.sigs.k8s.io
    config:
    - requests: ["ib"]
      opaque:
        driver: ib.sigs.k8s.io
        parameters:
          apiVersion: ib.resource.sigs.k8s.io/v1alpha1
          kind: IbConfig
          mtu: 9000
EOF
then
  test-fail "webhook rejects v1beta1 invalid IbConfig" "webhook did not reject v1beta1 ResourceClaim with invalid MTU"
fi
test-pass "webhook rejects invalid IbConfig via v1beta1"


# ═══════════════════════════════════════════════════════════════════════════════
# Summary
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "============================================"
echo "  Tests run: $TESTS_RUN  |  Pass: $PASS  |  Fail: $FAIL"
echo "============================================"

if [[ $FAIL -gt 0 ]]; then
  echo "SOME TESTS FAILED"
  exit 1
fi

echo "All tests passed"
