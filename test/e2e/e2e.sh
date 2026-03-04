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

# End-to-end test for the DRA example driver (IB profile)

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

function ib-devices-from-logs {
  local logs="$1"
  echo "$logs" | sed -nE "s/^declare -x IB_DEVICE_[[:digit:]]+=.(.+).$/\1/p"
}

function ib-device-env {
  local logs="$1"
  local key="$2"
  echo "$logs" | sed -nE "s/^declare -x ${key}=\"(.+)\"$/\1/p"
}

declare -a observed_devices
function device-already-seen {
  local dev="$1"
  for seen in "${observed_devices[@]}"; do
    if [[ "$dev" == "$seen" ]]; then return 0; fi
  done
  return 1
}

# ── Cluster readiness ────────────────────────────────────────────────────────

kind get clusters
kubectl get nodes
kubectl wait --for=condition=Ready nodes/dra-example-driver-cluster-worker --timeout=120s

# ── Webhook readiness ────────────────────────────────────────────────────────

# Even after verifying that the Pod is Ready and the expected Endpoints resource
# exists with the Pod's IP, the webhook still seems to have "connection refused"
# issues, so retry here until we can ensure it's available before the real tests
# start.
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
                expression: "device.attributes['type'].stringValue == 'VF'"
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

# Clean up any leftover resources from a previous run
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

# pod0/ctr0 — 1 IB device
ib_test1_pod0_ctr0_logs=$(kubectl logs -n ib-test1 pod0 -c ctr0)
ib_test1_pod0_ctr0_devices=$(ib-devices-from-logs "$ib_test1_pod0_ctr0_logs")
ib_test1_pod0_ctr0_count=$(echo "$ib_test1_pod0_ctr0_devices" | wc -w | tr -d ' ')
assert-eq "ib-test1 pod0/ctr0 device count" "1" "$ib_test1_pod0_ctr0_count"
ib_test1_pod0_ctr0_dev="$ib_test1_pod0_ctr0_devices"
if device-already-seen "$ib_test1_pod0_ctr0_dev"; then
  test-fail "ib-test1 pod0/ctr0 unique device" "device $ib_test1_pod0_ctr0_dev already claimed"
fi
echo "  pod0/ctr0 claimed $ib_test1_pod0_ctr0_dev"
observed_devices+=("$ib_test1_pod0_ctr0_dev")

# pod1/ctr0 — 1 IB device, different from pod0
ib_test1_pod1_ctr0_logs=$(kubectl logs -n ib-test1 pod1 -c ctr0)
ib_test1_pod1_ctr0_devices=$(ib-devices-from-logs "$ib_test1_pod1_ctr0_logs")
ib_test1_pod1_ctr0_count=$(echo "$ib_test1_pod1_ctr0_devices" | wc -w | tr -d ' ')
assert-eq "ib-test1 pod1/ctr0 device count" "1" "$ib_test1_pod1_ctr0_count"
ib_test1_pod1_ctr0_dev="$ib_test1_pod1_ctr0_devices"
if device-already-seen "$ib_test1_pod1_ctr0_dev"; then
  test-fail "ib-test1 pod1/ctr0 unique device" "device $ib_test1_pod1_ctr0_dev already claimed"
fi
echo "  pod1/ctr0 claimed $ib_test1_pod1_ctr0_dev"
observed_devices+=("$ib_test1_pod1_ctr0_dev")

test-pass "ib-test1: two pods each got a distinct VF"


# ═══════════════════════════════════════════════════════════════════════════════
# ib-test2: One pod, one container — IB VF with custom pkey + MTU config
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== ib-test2: Custom IB config (pkey + MTU) ==="

kubectl wait --for=condition=Ready -n ib-test2 pod/pod0 --timeout=120s

ib_test2_running=$(kubectl get pods -n ib-test2 --no-headers | grep -c 'Running')
assert-eq "ib-test2 pod count" "1" "$ib_test2_running"

ib_test2_pod0_ctr0_logs=$(kubectl logs -n ib-test2 pod0 -c ctr0)
ib_test2_pod0_ctr0_devices=$(ib-devices-from-logs "$ib_test2_pod0_ctr0_logs")
ib_test2_pod0_ctr0_count=$(echo "$ib_test2_pod0_ctr0_devices" | wc -w | tr -d ' ')
assert-eq "ib-test2 pod0/ctr0 device count" "1" "$ib_test2_pod0_ctr0_count"
ib_test2_pod0_ctr0_dev="$ib_test2_pod0_ctr0_devices"
if device-already-seen "$ib_test2_pod0_ctr0_dev"; then
  test-fail "ib-test2 pod0/ctr0 unique device" "device $ib_test2_pod0_ctr0_dev already claimed"
fi
echo "  pod0/ctr0 claimed $ib_test2_pod0_ctr0_dev"
observed_devices+=("$ib_test2_pod0_ctr0_dev")

# Verify pkey env var exists (pkey=32769 => 0x8001)
ib_test2_pkey=$(ib-device-env "$ib_test2_pod0_ctr0_logs" "IB_DEVICE_0_PKEY")
assert-eq "ib-test2 pod0/ctr0 pkey" "0x8001" "$ib_test2_pkey"

# Verify MTU env var exists (mtu=4096)
ib_test2_mtu=$(ib-device-env "$ib_test2_pod0_ctr0_logs" "IB_DEVICE_0_MTU")
assert-eq "ib-test2 pod0/ctr0 MTU" "4096" "$ib_test2_mtu"

test-pass "ib-test2: VF with custom pkey and MTU config"


# ═══════════════════════════════════════════════════════════════════════════════
# ib-test3: One pod, one container — IB PF with admin access
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== ib-test3: PF with admin access ==="

kubectl wait --for=condition=Ready -n ib-test3 pod/pod0 --timeout=120s

ib_test3_running=$(kubectl get pods -n ib-test3 --no-headers | grep -c 'Running')
assert-eq "ib-test3 pod count" "1" "$ib_test3_running"

ib_test3_pod0_ctr0_logs=$(kubectl logs -n ib-test3 pod0 -c ctr0)
ib_test3_pod0_ctr0_devices=$(ib-devices-from-logs "$ib_test3_pod0_ctr0_logs")
ib_test3_pod0_ctr0_count=$(echo "$ib_test3_pod0_ctr0_devices" | wc -w | tr -d ' ')
assert-eq "ib-test3 pod0/ctr0 device count" "1" "$ib_test3_pod0_ctr0_count"

# Admin access pods should have DRA_ADMIN_ACCESS=true
ib_test3_admin_access=$(ib-device-env "$ib_test3_pod0_ctr0_logs" "DRA_ADMIN_ACCESS")
assert-eq "ib-test3 pod0/ctr0 admin access" "true" "$ib_test3_admin_access"
echo "  pod0/ctr0 has admin access: $ib_test3_admin_access"

test-pass "ib-test3: PF with admin access"


# ═══════════════════════════════════════════════════════════════════════════════
# ib-test4: One pod, one container — 2 VFs on the same NUMA node
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== ib-test4: Dual VFs on same NUMA node ==="

kubectl wait --for=condition=Ready -n ib-test4 pod/pod0 --timeout=120s

ib_test4_running=$(kubectl get pods -n ib-test4 --no-headers | grep -c 'Running')
assert-eq "ib-test4 pod count" "1" "$ib_test4_running"

ib_test4_pod0_ctr0_logs=$(kubectl logs -n ib-test4 pod0 -c ctr0)
ib_test4_pod0_ctr0_devices=$(ib-devices-from-logs "$ib_test4_pod0_ctr0_logs")
ib_test4_pod0_ctr0_count=$(echo "$ib_test4_pod0_ctr0_devices" | wc -w | tr -d ' ')
assert-eq "ib-test4 pod0/ctr0 device count" "2" "$ib_test4_pod0_ctr0_count"

# Verify the two devices are different
ib_test4_dev1=$(echo "$ib_test4_pod0_ctr0_devices" | head -1)
ib_test4_dev2=$(echo "$ib_test4_pod0_ctr0_devices" | tail -1)
if [[ "$ib_test4_dev1" == "$ib_test4_dev2" ]]; then
  test-fail "ib-test4 distinct devices" "both devices are the same: $ib_test4_dev1"
fi
echo "  pod0/ctr0 claimed $ib_test4_dev1 and $ib_test4_dev2"

test-pass "ib-test4: two distinct VFs on the same NUMA node"


# ═══════════════════════════════════════════════════════════════════════════════
# ib-test5: One pod, one container — active port filter
# ═══════════════════════════════════════════════════════════════════════════════

echo ""
echo "=== ib-test5: Active port health filter ==="

kubectl wait --for=condition=Ready -n ib-test5 pod/pod0 --timeout=120s

ib_test5_running=$(kubectl get pods -n ib-test5 --no-headers | grep -c 'Running')
assert-eq "ib-test5 pod count" "1" "$ib_test5_running"

ib_test5_pod0_ctr0_logs=$(kubectl logs -n ib-test5 pod0 -c ctr0)
ib_test5_pod0_ctr0_devices=$(ib-devices-from-logs "$ib_test5_pod0_ctr0_logs")
ib_test5_pod0_ctr0_count=$(echo "$ib_test5_pod0_ctr0_devices" | wc -w | tr -d ' ')
assert-eq "ib-test5 pod0/ctr0 device count" "1" "$ib_test5_pod0_ctr0_count"
echo "  pod0/ctr0 claimed $ib_test5_pod0_ctr0_devices (active-port filtered)"

test-pass "ib-test5: device with active port filter"


# ═══════════════════════════════════════════════════════════════════════════════
# Cleanup: verify fast deletion (less than default 30s grace period)
# see https://github.com/kubernetes/kubernetes/issues/127188 for details
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

# Webhook should reject IbConfig with pkey=0 (invalid: 0x0000 not in 0x0001-0xFFFF)
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
  test-fail "webhook accepts valid IbConfig" "webhook rejected a valid IbConfig (pkey=0x8001, mtu=4096)"
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
