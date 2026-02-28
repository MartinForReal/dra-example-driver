/*
 * Copyright The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package ib implements the DRA profile for InfiniBand devices. It discovers
// IB PFs and VFs using libibverbs (cgo) and sysfs, auto-provisions VFs on
// baremetal hosts, and generates CDI container edits that move the IB netdev
// and RDMA device into the container's network namespace.
package ib

import (
	"context"
	"fmt"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "sigs.k8s.io/dra-example-driver/api/example.com/resource/ib/v1alpha1"
	"sigs.k8s.io/dra-example-driver/internal/ibverbs"
	"sigs.k8s.io/dra-example-driver/internal/netns"
	"sigs.k8s.io/dra-example-driver/internal/profiles"
	"sigs.k8s.io/dra-example-driver/internal/sriov"
	"sigs.k8s.io/dra-example-driver/internal/sysfs"
)

const ProfileName = "ib"

// DeviceEntry holds the combined ibverbs + sysfs info for a single IB port
// that will be published as an allocatable device.
type DeviceEntry struct {
	// DeviceName is the DRA device name (e.g., "mlx5_0-port1").
	DeviceName string
	// IBDevName is the IB device name (e.g., "mlx5_0").
	IBDevName string
	// PortNum is the 1-based port number.
	PortNum int
	// Type is "PF" or "VF".
	Type string
	// LinkSpeed is the effective link speed string (e.g., "100Gb/s").
	LinkSpeed string
	// PortState is the port state string (e.g., "Active").
	PortState string
	// FirmwareVersion is the HCA firmware version.
	FirmwareVersion string
	// NodeGUID is the device's node GUID.
	NodeGUID string
	// PortGUID is the port's GID (GID index 0).
	PortGUID string
	// NUMANode is the NUMA affinity (-1 if unknown).
	NUMANode int
	// PCIAddress is the PCI bus address.
	PCIAddress string
	// ParentDevice is the IB device name of the parent PF (for VFs).
	ParentDevice string
	// NetDevices is the list of network interface names.
	NetDevices []string
}

// Profile implements the DRA profile for InfiniBand devices.
type Profile struct {
	nodeName string
	numVFs   int

	// devices is populated after EnumerateDevices.
	devices []DeviceEntry
}

// NewProfile creates a new IB profile.
func NewProfile(nodeName string, numVFs int) *Profile {
	return &Profile{
		nodeName: nodeName,
		numVFs:   numVFs,
	}
}

// EnumerateDevices discovers IB hardware and publishes all PFs and VFs as
// allocatable DRA devices. On baremetal hosts with SR-IOV capable PFs, VFs are
// auto-provisioned at startup if numVFs > 0.
func (p *Profile) EnumerateDevices(ctx context.Context) (resourceslice.DriverResources, error) {
	logger := klog.FromContext(ctx)

	// Step 1: Auto-provision VFs on SR-IOV capable PFs (baremetal only).
	if p.numVFs > 0 {
		if err := p.provisionVFs(ctx); err != nil {
			logger.Error(err, "Failed to provision VFs, continuing with existing devices")
		}
	}

	// Step 2: Discover all IB devices using ibverbs.
	ibDevices, err := ibverbs.ListDevices()
	if err != nil {
		return resourceslice.DriverResources{}, fmt.Errorf("ibverbs.ListDevices: %w", err)
	}
	if len(ibDevices) == 0 {
		logger.Info("No InfiniBand devices found on this host")
		return resourceslice.DriverResources{}, nil
	}

	// Step 3: Augment with sysfs info (PF/VF type, NUMA, PCI, netdevs).
	sysfsDevices, err := sysfs.ListIBDevices()
	if err != nil {
		logger.Error(err, "Failed to read sysfs IB devices, using ibverbs info only")
	}
	sysfsMap := make(map[string]*sysfs.IBDeviceInfo)
	for i := range sysfsDevices {
		sysfsMap[sysfsDevices[i].Name] = &sysfsDevices[i]
	}

	// Find PCI -> IB device name mapping for parent PF resolution
	pciToIBDev := make(map[string]string)
	for _, si := range sysfsDevices {
		if si.PCIAddress != "" {
			pciToIBDev[si.PCIAddress] = si.Name
		}
	}

	// Step 4: Build device entries.
	var entries []DeviceEntry
	for _, ibDev := range ibDevices {
		si := sysfsMap[ibDev.Name]

		for _, port := range ibDev.Ports {
			entry := DeviceEntry{
				DeviceName:      fmt.Sprintf("%s-port%d", ibDev.Name, port.PortNum),
				IBDevName:       ibDev.Name,
				PortNum:         port.PortNum,
				LinkSpeed:       port.EffectiveSpeed(),
				PortState:       port.State.String(),
				FirmwareVersion: ibDev.FirmwareVersion,
				NodeGUID:        ibDev.NodeGUIDString(),
				NUMANode:        -1,
			}

			// Port GUID from GID
			gidBytes := port.GID[:]
			entry.PortGUID = formatGID(gidBytes)

			// Sysfs augmentation
			if si != nil {
				entry.PCIAddress = si.PCIAddress
				entry.NUMANode = si.NUMANode
				entry.NetDevices = si.NetDevices

				if si.IsVF {
					entry.Type = "VF"
					if si.ParentPF != "" {
						if parentIBDev, ok := pciToIBDev[si.ParentPF]; ok {
							entry.ParentDevice = parentIBDev
						}
					}
				} else {
					entry.Type = "PF"
				}
			} else {
				entry.Type = "PF" // Default to PF if sysfs info unavailable
			}

			entries = append(entries, entry)
		}
	}

	p.devices = entries

	// Step 5: Build DRA DriverResources.
	var devices []resourceapi.Device
	for _, e := range entries {
		dev := resourceapi.Device{
			Name: e.DeviceName,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"type": {
					StringValue: ptr.To(e.Type),
				},
				"linkSpeed": {
					StringValue: ptr.To(e.LinkSpeed),
				},
				"portState": {
					StringValue: ptr.To(e.PortState),
				},
				"firmwareVersion": {
					StringValue: ptr.To(e.FirmwareVersion),
				},
				"nodeGUID": {
					StringValue: ptr.To(e.NodeGUID),
				},
				"portGUID": {
					StringValue: ptr.To(e.PortGUID),
				},
				"numaNode": {
					IntValue: ptr.To(int64(e.NUMANode)),
				},
				"pciAddress": {
					StringValue: ptr.To(e.PCIAddress),
				},
			},
		}
		if e.ParentDevice != "" {
			dev.Attributes["parentDevice"] = resourceapi.DeviceAttribute{
				StringValue: ptr.To(e.ParentDevice),
			}
		}
		devices = append(devices, dev)
	}

	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.nodeName: {
				Slices: []resourceslice.Slice{
					{
						Devices: devices,
					},
				},
			},
		},
	}

	logger.Info("Enumerated IB devices", "count", len(devices), "node", p.nodeName)
	return resources, nil
}

// provisionVFs auto-creates VFs on all SR-IOV capable PFs.
func (p Profile) provisionVFs(ctx context.Context) error {
	logger := klog.FromContext(ctx)

	pfs, err := sriov.DiscoverSRIOVPFs()
	if err != nil {
		return fmt.Errorf("discover SR-IOV PFs: %w", err)
	}

	if len(pfs) == 0 {
		logger.Info("No SR-IOV capable PFs found; running in VM mode or no SRIOV support")
		return nil
	}

	for _, pf := range pfs {
		desired := p.numVFs
		if desired > pf.TotalVFs {
			desired = pf.TotalVFs
		}
		logger.Info("Provisioning VFs", "pf", pf.IBDevName, "pciAddr", pf.PCIAddress, "desired", desired, "totalVFs", pf.TotalVFs)
		if err := sriov.ProvisionVFs(ctx, pf.PCIAddress, desired); err != nil {
			return fmt.Errorf("provision VFs on %s: %w", pf.PCIAddress, err)
		}
	}
	return nil
}

// SchemeBuilder implements [profiles.ConfigHandler].
func (p Profile) SchemeBuilder() runtime.SchemeBuilder {
	return runtime.NewSchemeBuilder(
		configapi.AddToScheme,
	)
}

// Validate implements [profiles.ConfigHandler].
func (p Profile) Validate(config runtime.Object) error {
	ibConfig, ok := config.(*configapi.IbConfig)
	if !ok {
		return fmt.Errorf("expected v1alpha1.IbConfig but got: %T", config)
	}
	return ibConfig.Validate()
}

// ApplyConfig implements [profiles.ConfigHandler].
func (p Profile) ApplyConfig(config runtime.Object, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	if config == nil {
		config = configapi.DefaultIbConfig()
	}
	if config, ok := config.(*configapi.IbConfig); ok {
		return applyIbConfig(config, results)
	}
	return nil, fmt.Errorf("runtime object is not a recognized configuration")
}

// applyIbConfig applies the IB configuration to allocated devices and returns
// CDI container edits for each device. The edits include environment variables
// describing the device and CDI hooks to move the netdev into the container's
// network namespace at runtime.
func applyIbConfig(config *configapi.IbConfig, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	perDeviceEdits := make(profiles.PerDeviceCDIContainerEdits)

	if err := config.Normalize(); err != nil {
		return nil, fmt.Errorf("error normalizing IB config: %w", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("error validating IB config: %w", err)
	}

	for i, result := range results {
		envs := []string{
			fmt.Sprintf("IB_DEVICE_%d=%s", i, result.Device),
		}

		// Parse IB device name and port from the device name (e.g., "mlx5_0-port1")
		parts := strings.SplitN(result.Device, "-port", 2)
		if len(parts) == 2 {
			envs = append(envs, fmt.Sprintf("IB_DEVICE_%d_IBDEV=%s", i, parts[0]))
			envs = append(envs, fmt.Sprintf("IB_DEVICE_%d_PORT=%s", i, parts[1]))
		}

		// Config-specific env vars
		if config.Pkey != nil {
			envs = append(envs, fmt.Sprintf("IB_DEVICE_%d_PKEY=0x%04X", i, *config.Pkey))
		}
		if config.TrafficClass != nil {
			envs = append(envs, fmt.Sprintf("IB_DEVICE_%d_TRAFFIC_CLASS=%d", i, *config.TrafficClass))
		}
		if config.MTU != nil {
			envs = append(envs, fmt.Sprintf("IB_DEVICE_%d_MTU=%d", i, *config.MTU))
		}

		edits := &cdispec.ContainerEdits{
			Env: envs,
		}

		// Add CDI hooks to move netdev into container namespace at runtime.
		// The hook is executed by the container runtime at createRuntime time.
		// We use the plugin binary itself as the hook helper — it's re-invoked
		// with the "move-netdev" subcommand.
		//
		// The actual netdev name is determined at prepare time (stored in the
		// checkpoint) and the hook args include it. We add a placeholder here
		// that gets resolved when the CDI spec is written.
		//
		// For now, we inject the IB device/port info as env vars. The actual
		// netdev move happens via the CDI hook configured at CDI spec write time.
		hookPath := "/usr/bin/dra-example-kubeletplugin"
		if len(parts) == 2 {
			ibDevName := parts[0]
			edits.Hooks = []*cdispec.Hook{
				{
					HookName: "createRuntime",
					Path:     hookPath,
					Args: []string{
						hookPath,
						"move-netdev",
						"--ib-dev", ibDevName,
					},
				},
			}
		}

		perDeviceEdits[result.Device] = &cdiapi.ContainerEdits{ContainerEdits: edits}
	}

	return perDeviceEdits, nil
}

// formatGID formats a 16-byte GID into the standard colon-separated hex format.
func formatGID(gid []byte) string {
	if len(gid) != 16 {
		return ""
	}
	parts := make([]string, 8)
	for i := 0; i < 8; i++ {
		parts[i] = fmt.Sprintf("%02x%02x", gid[i*2], gid[i*2+1])
	}
	return strings.Join(parts, ":")
}

// GetDeviceEntryByName looks up a DeviceEntry from the enumerated devices.
func (p *Profile) GetDeviceEntryByName(name string) (*DeviceEntry, bool) {
	for _, d := range p.devices {
		if d.DeviceName == name {
			return &d, true
		}
	}
	return nil, false
}

// MoveNetdevHookHelper is the function called when the plugin binary is
// invoked with the "move-netdev" subcommand by a CDI hook. It moves the
// IB netdev and RDMA device into the specified container's network namespace.
func MoveNetdevHookHelper(ctx context.Context, ibDevName string, containerPID int) error {
	logger := klog.FromContext(ctx)

	// Find network devices for this IB device
	devInfo, err := sysfs.GetIBDeviceInfo(ibDevName)
	if err != nil {
		return fmt.Errorf("get sysfs info for %s: %w", ibDevName, err)
	}

	for _, netDev := range devInfo.NetDevices {
		if err := netns.MoveNetdevToContainerNetns(ctx, netDev, containerPID); err != nil {
			return fmt.Errorf("move netdev %s: %w", netDev, err)
		}
	}

	// Move RDMA device
	if err := netns.MoveRDMADevToContainerNetns(ctx, ibDevName, containerPID); err != nil {
		logger.Error(err, "Failed to move RDMA device to container netns, continuing", "rdmaDev", ibDevName)
		// Non-fatal: RDMA namespace move may not be supported on all kernels
	}

	return nil
}
