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

// Package ibinventory implements the DRANET inventoryDB interface for
// InfiniBand devices. It discovers IB PFs and VFs using libibverbs (cgo)
// and sysfs, optionally auto-provisions VFs on baremetal hosts, and
// publishes them as DRA devices via the DRANET driver framework.
package ibinventory

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/dranet/pkg/apis"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/MartinForReal/dra-infiniband-driver/internal/ibverbs"
	"github.com/MartinForReal/dra-infiniband-driver/internal/sriov"
	"github.com/MartinForReal/dra-infiniband-driver/internal/sysfs"
)

const (
	// IB-specific attribute constants under the dra.net prefix.
	AttrIBType            = "dra.net/ibType"
	AttrIBLinkSpeed       = "dra.net/ibLinkSpeed"
	AttrIBPortState       = "dra.net/ibPortState"
	AttrIBFirmwareVersion = "dra.net/ibFirmwareVersion"
	AttrIBNodeGUID        = "dra.net/ibNodeGUID"
	AttrIBPortGUID        = "dra.net/ibPortGUID"
	AttrIBParentDevice    = "dra.net/ibParentDevice"
	AttrIBDevName         = "dra.net/ibDevName"

	defaultPollInterval = 30 * time.Second
)

// DeviceEntry holds the combined ibverbs + sysfs info for a single IB port.
type DeviceEntry struct {
	DeviceName      string
	IBDevName       string
	PortNum         int
	Type            string
	LinkSpeed       string
	PortState       string
	FirmwareVersion string
	NodeGUID        string
	PortGUID        string
	NUMANode        int
	PCIAddress      string
	ParentDevice    string
	NetDevices      []string
}

// DB implements the DRANET inventoryDB interface for InfiniBand devices.
//
// The interface contract (from github.com/google/dranet/pkg/driver):
//
//	Run(context.Context) error
//	GetResources(context.Context) <-chan []resourceapi.Device
//	GetNetInterfaceName(string) (string, error)
//	GetDeviceConfig(deviceName string) (*apis.NetworkConfig, bool)
//	AddPodNetNs(podKey string, netNs string)
//	RemovePodNetNs(podKey string)
//	GetPodNetNs(podKey string) (netNs string)
type DB struct {
	numVFs        int
	numSimDevices int

	mu            sync.RWMutex
	deviceStore   map[string]DeviceEntry
	podNetNsStore map[string]string

	notifications chan []resourceapi.Device
	pollInterval  time.Duration
}

// Option configures the DB.
type Option func(*DB)

// WithNumVFs sets the number of VFs to auto-provision per PF.
func WithNumVFs(n int) Option {
	return func(db *DB) { db.numVFs = n }
}

// WithNumSimDevices sets the number of simulated IB devices for testing.
func WithNumSimDevices(n int) Option {
	return func(db *DB) { db.numSimDevices = n }
}

// WithPollInterval overrides the default polling interval.
func WithPollInterval(d time.Duration) Option {
	return func(db *DB) { db.pollInterval = d }
}

// New creates a new IB inventory database.
func New(opts ...Option) *DB {
	db := &DB{
		deviceStore:   make(map[string]DeviceEntry),
		podNetNsStore: make(map[string]string),
		notifications: make(chan []resourceapi.Device),
		pollInterval:  defaultPollInterval,
	}
	for _, o := range opts {
		o(db)
	}
	return db
}

// Run starts the inventory loop. It discovers IB devices on startup,
// optionally provisions VFs, and then periodically re-discovers.
// This satisfies the inventoryDB.Run interface.
func (db *DB) Run(ctx context.Context) error {
	defer close(db.notifications)

	// Auto-provision VFs on first run.
	if db.numVFs > 0 {
		if err := db.provisionVFs(ctx); err != nil {
			klog.Errorf("IB inventory: failed to provision VFs: %v", err)
		}
	}

	// Initial scan.
	devices := db.scan(ctx)
	if len(devices) > 0 {
		db.notifications <- devices
	}

	// Periodic rescan.
	ticker := time.NewTicker(db.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			devices := db.scan(ctx)
			if len(devices) > 0 {
				db.notifications <- devices
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// GetResources returns the channel on which device updates are published.
func (db *DB) GetResources(ctx context.Context) <-chan []resourceapi.Device {
	return db.notifications
}

// GetNetInterfaceName returns the first network interface name for a device.
// For simulated devices, it re-creates the dummy interface if it was consumed
// (moved to a pod netns) so that DRANET can retry operations idempotently.
func (db *DB) GetNetInterfaceName(deviceName string) (string, error) {
	db.mu.RLock()
	entry, ok := db.deviceStore[deviceName]
	db.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("IB device %s not found in inventory", deviceName)
	}
	if len(entry.NetDevices) == 0 {
		return "", fmt.Errorf("IB device %s has no associated network interfaces", deviceName)
	}

	ifName := entry.NetDevices[0]

	// For simulated devices, ensure the dummy interface exists on the host.
	// It may have been consumed (moved to a pod netns) on a previous attempt.
	if db.numSimDevices > 0 {
		if err := exec.Command("ip", "link", "show", ifName).Run(); err != nil {
			// Re-create the dummy interface
			createDummyInterface(context.Background(), ifName)
		}
	}

	return ifName, nil
}

// GetDeviceConfig returns the DRANET NetworkConfig for a device.
// For IB devices we always return nil (no DRANET-level network config) since
// IB configuration is handled through our own IbConfig opaque parameters.
func (db *DB) GetDeviceConfig(deviceName string) (*apis.NetworkConfig, bool) {
	return nil, false
}

// AddPodNetNs stores a pod's network namespace path.
func (db *DB) AddPodNetNs(podKey string, netNs string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.podNetNsStore[podKey] = netNs
}

// RemovePodNetNs removes a pod's network namespace mapping.
func (db *DB) RemovePodNetNs(podKey string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	delete(db.podNetNsStore, podKey)
}

// GetPodNetNs retrieves a pod's network namespace path.
func (db *DB) GetPodNetNs(podKey string) string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.podNetNsStore[podKey]
}

// GetDeviceEntry returns the IB device entry for a given device name.
func (db *DB) GetDeviceEntry(deviceName string) (DeviceEntry, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	e, ok := db.deviceStore[deviceName]
	return e, ok
}

// scan discovers all IB devices and returns them as DRA devices.
func (db *DB) scan(ctx context.Context) []resourceapi.Device {
	logger := klog.FromContext(ctx)

	ibDevices, err := ibverbs.ListDevices()
	if err != nil {
		logger.Error(err, "IB inventory: ibverbs.ListDevices failed")
		return nil
	}

	if len(ibDevices) == 0 {
		logger.Info("IB inventory: no InfiniBand devices found")
		if db.numSimDevices > 0 {
			return db.scanSimulated(ctx)
		}
		return nil
	}

	// Augment with sysfs info.
	sysfsDevices, err := sysfs.ListIBDevices()
	if err != nil {
		logger.Error(err, "IB inventory: sysfs.ListIBDevices failed, using ibverbs info only")
	}
	sysfsMap := make(map[string]*sysfs.IBDeviceInfo)
	pciToIBDev := make(map[string]string)
	for i := range sysfsDevices {
		sysfsMap[sysfsDevices[i].Name] = &sysfsDevices[i]
		if sysfsDevices[i].PCIAddress != "" {
			pciToIBDev[sysfsDevices[i].PCIAddress] = sysfsDevices[i].Name
		}
	}

	var entries []DeviceEntry
	for _, ibDev := range ibDevices {
		si := sysfsMap[ibDev.Name]
		for _, port := range ibDev.Ports {
			entry := DeviceEntry{
				DeviceName:      sanitizeDeviceName(fmt.Sprintf("%s-port%d", ibDev.Name, port.PortNum)),
				IBDevName:       ibDev.Name,
				PortNum:         port.PortNum,
				LinkSpeed:       port.EffectiveSpeed(),
				PortState:       port.State.String(),
				FirmwareVersion: ibDev.FirmwareVersion,
				NodeGUID:        ibDev.NodeGUIDString(),
				NUMANode:        -1,
			}

			gidBytes := port.GID[:]
			entry.PortGUID = formatGID(gidBytes)

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
				entry.Type = "PF"
			}

			entries = append(entries, entry)
		}
	}

	// Update store.
	devices := db.entriesToDevices(entries)
	db.updateStore(entries)

	logger.Info("IB inventory scan complete", "deviceCount", len(devices))
	return devices
}

// scanSimulated creates simulated IB devices for testing.
// It creates real dummy network interfaces so that DRANET's netlink-based
// prepare flow (GetNetInterfaceName → LinkByName → netns move) works.
func (db *DB) scanSimulated(ctx context.Context) []resourceapi.Device {
	logger := klog.FromContext(ctx)
	logger.Info("IB inventory: creating simulated IB devices", "count", db.numSimDevices)

	var entries []DeviceEntry

	// Create dummy network interfaces for simulated devices.
	// DRANET requires real netlink-resolvable interfaces to move into pod netns.
	pfNetdev := "simib0"
	createDummyInterface(ctx, pfNetdev)

	// Simulated PF.
	entries = append(entries, DeviceEntry{
		DeviceName:      "sim-mlx5-0-port1",
		IBDevName:       "sim_mlx5_0",
		PortNum:         1,
		Type:            "PF",
		LinkSpeed:       "100Gb/s",
		PortState:       "Active",
		FirmwareVersion: "20.99.0000",
		NodeGUID:        "0000:0000:0000:0001",
		PortGUID:        "0000:0000:0000:0001",
		NUMANode:        0,
		PCIAddress:      "0000:00:00.0",
		NetDevices:      []string{pfNetdev},
	})

	for i := 1; i <= db.numSimDevices; i++ {
		vfNetdev := fmt.Sprintf("simib%d", i)
		createDummyInterface(ctx, vfNetdev)
		entries = append(entries, DeviceEntry{
			DeviceName:      fmt.Sprintf("sim-mlx5-%d-port1", i),
			IBDevName:       fmt.Sprintf("sim_mlx5_%d", i),
			PortNum:         1,
			Type:            "VF",
			LinkSpeed:       "100Gb/s",
			PortState:       "Active",
			FirmwareVersion: "20.99.0000",
			NodeGUID:        fmt.Sprintf("0000:0000:0000:%04x", i+1),
			PortGUID:        fmt.Sprintf("0000:0000:0000:%04x", i+1),
			NUMANode:        0,
			PCIAddress:      fmt.Sprintf("0000:00:00.%d", i),
			ParentDevice:    "sim_mlx5_0",
			NetDevices:      []string{vfNetdev},
		})
	}

	devices := db.entriesToDevices(entries)
	db.updateStore(entries)
	return devices
}

// createDummyInterface creates a Linux dummy network interface for testing.
// If the interface already exists, this is a no-op.
func createDummyInterface(ctx context.Context, name string) {
	logger := klog.FromContext(ctx)
	// Check if already exists
	if err := exec.Command("ip", "link", "show", name).Run(); err == nil {
		return // already exists
	}
	cmd := exec.Command("ip", "link", "add", name, "type", "dummy")
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Error(err, "Failed to create dummy interface", "name", name, "output", string(out))
		return
	}
	cmd = exec.Command("ip", "link", "set", name, "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Error(err, "Failed to bring up dummy interface", "name", name, "output", string(out))
	}
	logger.V(2).Info("Created dummy interface", "name", name)
}

// entriesToDevices converts DeviceEntry slice to DRA Device slice.
func (db *DB) entriesToDevices(entries []DeviceEntry) []resourceapi.Device {
	var devices []resourceapi.Device
	for _, e := range entries {
		dev := resourceapi.Device{
			Name: e.DeviceName,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				// Standard DRANET attributes
				resourceapi.QualifiedName(apis.AttrNUMANode): {
					IntValue: ptr.To(int64(e.NUMANode)),
				},
				resourceapi.QualifiedName(apis.AttrPCIAddress): {
					StringValue: ptr.To(e.PCIAddress),
				},
				resourceapi.QualifiedName(apis.AttrRDMA): {
					BoolValue: ptr.To(true),
				},

				// IB-specific attributes
				resourceapi.QualifiedName(AttrIBType): {
					StringValue: ptr.To(e.Type),
				},
				resourceapi.QualifiedName(AttrIBLinkSpeed): {
					StringValue: ptr.To(e.LinkSpeed),
				},
				resourceapi.QualifiedName(AttrIBPortState): {
					StringValue: ptr.To(e.PortState),
				},
				resourceapi.QualifiedName(AttrIBFirmwareVersion): {
					StringValue: ptr.To(e.FirmwareVersion),
				},
				resourceapi.QualifiedName(AttrIBNodeGUID): {
					StringValue: ptr.To(e.NodeGUID),
				},
				resourceapi.QualifiedName(AttrIBPortGUID): {
					StringValue: ptr.To(e.PortGUID),
				},
				resourceapi.QualifiedName(AttrIBDevName): {
					StringValue: ptr.To(e.IBDevName),
				},
			},
		}

		// Network interface name (DRANET standard attribute).
		if len(e.NetDevices) > 0 {
			dev.Attributes[resourceapi.QualifiedName(apis.AttrInterfaceName)] = resourceapi.DeviceAttribute{
				StringValue: ptr.To(e.NetDevices[0]),
			}
		}

		if e.ParentDevice != "" {
			dev.Attributes[resourceapi.QualifiedName(AttrIBParentDevice)] = resourceapi.DeviceAttribute{
				StringValue: ptr.To(e.ParentDevice),
			}
		}

		devices = append(devices, dev)
	}
	return devices
}

// updateStore replaces the device store with the latest scan results.
func (db *DB) updateStore(entries []DeviceEntry) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.deviceStore = make(map[string]DeviceEntry, len(entries))
	for _, e := range entries {
		db.deviceStore[e.DeviceName] = e
	}
}

// provisionVFs auto-creates VFs on all SR-IOV capable PFs.
func (db *DB) provisionVFs(ctx context.Context) error {
	logger := klog.FromContext(ctx)

	pfs, err := sriov.DiscoverSRIOVPFs()
	if err != nil {
		return fmt.Errorf("discover SR-IOV PFs: %w", err)
	}
	if len(pfs) == 0 {
		logger.Info("IB inventory: no SR-IOV capable PFs found")
		return nil
	}

	for _, pf := range pfs {
		desired := db.numVFs
		if desired > pf.TotalVFs {
			desired = pf.TotalVFs
		}
		logger.Info("Provisioning VFs", "pf", pf.IBDevName, "pciAddr", pf.PCIAddress, "desired", desired)
		if err := sriov.ProvisionVFs(ctx, pf.PCIAddress, desired); err != nil {
			return fmt.Errorf("provision VFs on %s: %w", pf.PCIAddress, err)
		}
	}
	return nil
}

// sanitizeDeviceName converts a device name to be RFC 1123 DNS label compliant.
// ResourceSlice device names must match [a-z0-9]([-a-z0-9]*[a-z0-9])?.
func sanitizeDeviceName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "_", "-")
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
