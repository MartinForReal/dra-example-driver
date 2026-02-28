# InfiniBand DRA Resource Driver for Kubernetes

This repository contains a Dynamic Resource Allocation (DRA) resource driver for
[InfiniBand](https://docs.nebius.com/kubernetes/gpu/clusters) devices on
Kubernetes.

It uses the [Dynamic Resource Allocation
(DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
feature of Kubernetes to discover, allocate, and configure InfiniBand (IB)
devices for containerized workloads. The driver supports both **Virtual
Functions (VFs) in VMs** and **Physical Functions (PFs) on baremetal machines**,
with automatic VF provisioning on baremetal hosts.

## Features

- **Real hardware discovery** via `libibverbs` (cgo) and Linux `sysfs`
- **Auto-detection of VM vs baremetal** based on SR-IOV capabilities
- **Automatic VF provisioning** on baremetal hosts at startup (pre-create pool)
- **Network namespace isolation** — IB netdev moved into container's netns
- **RDMA namespace isolation** — dedicated RDMA namespace per container (exclusive mode)
- **Topology-aware scheduling** — exposes NUMA node and PCI address for GPUDirect RDMA affinity
- **Configurable via opaque device config** — partition key (pkey), traffic class (QoS), MTU
- **CEL-based device selection** — filter by device type (PF/VF), port state, link speed, NUMA node, etc.

## Device Attributes

Each IB device (per-port) is published as a DRA device with the following attributes:

| Attribute | Type | Description |
|-----------|------|-------------|
| `type` | string | `"PF"` or `"VF"` |
| `linkSpeed` | string | Effective link speed, e.g., `"100Gb/s"` |
| `portState` | string | `"Active"`, `"Down"`, `"Init"`, `"Armed"` |
| `firmwareVersion` | string | HCA firmware version |
| `nodeGUID` | string | Device node GUID |
| `portGUID` | string | Port GID (index 0) |
| `numaNode` | int | NUMA node affinity (-1 if unknown) |
| `pciAddress` | string | PCI bus address |
| `parentDevice` | string | Parent PF IB device name (only for VFs) |

## Configuration (IbConfig)

Users can optionally specify an opaque device configuration in their
ResourceClaim to customize IB device params:

```yaml
config:
- opaque:
    driver: ib.sigs.k8s.io
    parameters:
      apiVersion: ib.resource.sigs.k8s.io/v1alpha1
      kind: IbConfig
      pkey: 32769        # 0x8001 — limited membership partition key
      trafficClass: 128  # QoS traffic class
      mtu: 4096          # IB MTU
```

All fields are optional. When not specified, fabric/port defaults are used.

## Architecture

```
┌─────────────────────────────────────────┐
│           Kubernetes Cluster            │
│                                         │
│  ┌───────────────────────────────────┐  │
│  │   dra-example-kubeletplugin       │  │
│  │   (DaemonSet per node)            │  │
│  │                                   │  │
│  │  ┌─────────┐  ┌──────────────┐   │  │
│  │  │ ibverbs │  │    sysfs     │   │  │
│  │  │  (cgo)  │  │  (PF/VF/    │   │  │
│  │  │         │  │   NUMA/PCI) │   │  │
│  │  └────┬────┘  └──────┬──────┘   │  │
│  │       └──────┬───────┘          │  │
│  │              ▼                   │  │
│  │    ┌─────────────────┐          │  │
│  │    │  IB Profile     │          │  │
│  │    │  (enumerate +   │          │  │
│  │    │   provision VFs)│          │  │
│  │    └────────┬────────┘          │  │
│  │             ▼                   │  │
│  │    ┌─────────────────┐          │  │
│  │    │  ResourceSlice  │──publish─┼──┼──▶ API Server
│  │    └─────────────────┘          │  │
│  │             ▼                   │  │
│  │    ┌─────────────────┐          │  │
│  │    │  CDI Spec Gen   │          │  │
│  │    │  (netdev move + │          │  │
│  │    │   env vars)     │          │  │
│  │    └─────────────────┘          │  │
│  └───────────────────────────────────┘  │
│                                         │
│  ┌───────────────────────────────────┐  │
│  │   dra-example-webhook             │  │
│  │   (Deployment, validates IbConfig)│  │
│  └───────────────────────────────────┘  │
└─────────────────────────────────────────┘
```

## Quickstart

### Prerequisites

* Kubernetes 1.35+ with DRA feature gate enabled
* Nodes with Mellanox InfiniBand HCAs
* `libibverbs` and `rdma-core` on nodes (or use the containerized driver image)
* [helm v3.7.0+](https://helm.sh/docs/intro/install/)

### Install

```bash
helm upgrade -i \
  --create-namespace \
  --namespace dra-ib-driver \
  dra-ib-driver \
  deployments/helm/dra-example-driver \
  --set kubeletPlugin.numVFs=8  # Set to 0 for VM mode (no VF provisioning)
```

### Verify

Check the driver pods are running:
```bash
kubectl get pod -n dra-ib-driver
```

Check discovered IB devices:
```bash
kubectl get resourceslice -o yaml
```

### Example: Request 1 IB VF

```bash
kubectl apply -f demo/ib-test1.yaml
```

### Example: Request IB VF with custom pkey and MTU

```bash
kubectl apply -f demo/ib-test2.yaml
```

### Example: Request IB PF with admin access (baremetal)

```bash
kubectl apply -f demo/ib-test3.yaml
```

### Example: 2 IB VFs on same NUMA node (GPUDirect RDMA affinity)

```bash
kubectl apply -f demo/ib-test4.yaml
```

### Example: Only active ports

```bash
kubectl apply -f demo/ib-test5.yaml
```

### Clean Up

```bash
kubectl delete --wait=false -f demo/ib-test{1,2,3,4,5}.yaml
```

## VM vs Baremetal Mode

The driver auto-detects whether it's running on a VM or baremetal based on
SR-IOV capabilities in sysfs:

- **Baremetal**: PFs with `sriov_totalvfs > 0` are detected. If `numVFs > 0`,
  VFs are pre-created at startup. Both PFs and VFs are advertised.
- **VM**: Only VFs (passed through from the hypervisor) are detected and
  advertised. No VF provisioning is attempted.

## Building

```bash
# Build binaries (requires libibverbs-dev)
make cmds

# Build container image
./demo/build-driver.sh

# Run tests
make test
```

## References

* [Dynamic Resource Allocation in Kubernetes](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
* [Container Device Interface (CDI)](https://github.com/cncf-tags/container-device-interface)

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://slack.k8s.io/)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/dev)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).
