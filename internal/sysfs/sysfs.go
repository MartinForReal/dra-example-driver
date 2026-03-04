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

// Package sysfs provides helpers to read InfiniBand and SR-IOV information
// from the Linux sysfs filesystem.
package sysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	sysClassInfiniband = "/sys/class/infiniband"
	sysClassNet        = "/sys/class/net"
	sysBusPCI          = "/sys/bus/pci/devices"
)

// IBDeviceInfo holds information gathered from sysfs about an IB device.
type IBDeviceInfo struct {
	// Name is the IB device name (e.g., mlx5_0).
	Name string
	// PCIAddress is the PCI bus address (e.g., 0000:3b:00.0).
	PCIAddress string
	// NUMANode is the NUMA node affinity (-1 if unknown).
	NUMANode int
	// IsPF is true if this is a Physical Function.
	IsPF bool
	// IsVF is true if this is a Virtual Function.
	IsVF bool
	// SRIOVTotalVFs is the total number of VFs this PF supports (0 for non-PFs).
	SRIOVTotalVFs int
	// SRIOVNumVFs is the current number of active VFs (0 for non-PFs).
	SRIOVNumVFs int
	// ParentPF is the PCI address of the parent PF (empty for PFs).
	ParentPF string
	// NetDevices is the list of network interface names associated with this IB device.
	NetDevices []string
	// NodeGUID from sysfs.
	NodeGUID string
	// PortGUIDs maps port number to the port GUID read from sysfs.
	PortGUIDs map[int]string
}

// ListIBDevices discovers all InfiniBand devices from sysfs.
func ListIBDevices() ([]IBDeviceInfo, error) {
	entries, err := os.ReadDir(sysClassInfiniband)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", sysClassInfiniband, err)
	}

	var devices []IBDeviceInfo
	for _, entry := range entries {
		devName := entry.Name()
		info, err := GetIBDeviceInfo(devName)
		if err != nil {
			continue
		}
		devices = append(devices, *info)
	}
	return devices, nil
}

// GetIBDeviceInfo reads detailed information about a single IB device from sysfs.
func GetIBDeviceInfo(devName string) (*IBDeviceInfo, error) {
	devPath := filepath.Join(sysClassInfiniband, devName)

	info := &IBDeviceInfo{
		Name:      devName,
		NUMANode:  -1,
		PortGUIDs: make(map[int]string),
	}

	// Resolve PCI device path
	deviceLink := filepath.Join(devPath, "device")
	pciPath, err := filepath.EvalSymlinks(deviceLink)
	if err == nil {
		info.PCIAddress = filepath.Base(pciPath)

		// Read NUMA node
		info.NUMANode = readIntFile(filepath.Join(pciPath, "numa_node"), -1)

		// Determine PF/VF status
		info.IsPF = isPF(pciPath)
		info.IsVF = isVF(pciPath)

		if info.IsPF {
			info.SRIOVTotalVFs = readIntFile(filepath.Join(pciPath, "sriov_totalvfs"), 0)
			info.SRIOVNumVFs = readIntFile(filepath.Join(pciPath, "sriov_numvfs"), 0)
		}

		if info.IsVF {
			physfnLink := filepath.Join(pciPath, "physfn")
			pfPath, err := filepath.EvalSymlinks(physfnLink)
			if err == nil {
				info.ParentPF = filepath.Base(pfPath)
			}
		}
	}

	// Read node_guid
	info.NodeGUID = readStringFile(filepath.Join(devPath, "node_guid"))

	// Read port GUIDs
	portsPath := filepath.Join(devPath, "ports")
	portEntries, err := os.ReadDir(portsPath)
	if err == nil {
		for _, pe := range portEntries {
			portNum, err := strconv.Atoi(pe.Name())
			if err != nil {
				continue
			}
			gidPath := filepath.Join(portsPath, pe.Name(), "gids", "0")
			gid := readStringFile(gidPath)
			if gid != "" {
				info.PortGUIDs[portNum] = gid
			}
		}
	}

	// Find associated network devices
	info.NetDevices = findNetDevices(devName)

	return info, nil
}

// GetSRIOVTotalVFs returns the total number of VFs supported by a PCI device.
func GetSRIOVTotalVFs(pciAddr string) (int, error) {
	path := filepath.Join(sysBusPCI, pciAddr, "sriov_totalvfs")
	return readIntFileErr(path)
}

// GetSRIOVNumVFs returns the current number of VFs enabled for a PCI device.
func GetSRIOVNumVFs(pciAddr string) (int, error) {
	path := filepath.Join(sysBusPCI, pciAddr, "sriov_numvfs")
	return readIntFileErr(path)
}

// SetSRIOVNumVFs sets the number of VFs for a PCI device.
func SetSRIOVNumVFs(pciAddr string, count int) error {
	path := filepath.Join(sysBusPCI, pciAddr, "sriov_numvfs")
	return os.WriteFile(path, []byte(strconv.Itoa(count)), 0644)
}

// IsPF checks if the given PCI device is a Physical Function that supports SR-IOV.
func IsPF(pciAddr string) bool {
	return isPF(filepath.Join(sysBusPCI, pciAddr))
}

// IsVF checks if the given PCI device is a Virtual Function.
func IsVF(pciAddr string) bool {
	return isVF(filepath.Join(sysBusPCI, pciAddr))
}

// GetParentPF returns the PCI address of the parent PF for a VF.
func GetParentPF(vfPCIAddr string) (string, error) {
	physfnLink := filepath.Join(sysBusPCI, vfPCIAddr, "physfn")
	pfPath, err := filepath.EvalSymlinks(physfnLink)
	if err != nil {
		return "", fmt.Errorf("read physfn symlink for %s: %w", vfPCIAddr, err)
	}
	return filepath.Base(pfPath), nil
}

// ListVFs returns the PCI addresses of all VFs belonging to a PF.
func ListVFs(pfPCIAddr string) ([]string, error) {
	pfPath := filepath.Join(sysBusPCI, pfPCIAddr)
	entries, err := os.ReadDir(pfPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pfPath, err)
	}

	var vfs []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "virtfn") {
			continue
		}
		vfLink := filepath.Join(pfPath, name)
		vfPath, err := filepath.EvalSymlinks(vfLink)
		if err != nil {
			continue
		}
		vfs = append(vfs, filepath.Base(vfPath))
	}
	return vfs, nil
}

// FindIBDeviceByPCI finds the InfiniBand device name for a given PCI address.
func FindIBDeviceByPCI(pciAddr string) (string, error) {
	entries, err := os.ReadDir(sysClassInfiniband)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", sysClassInfiniband, err)
	}

	for _, entry := range entries {
		devLink := filepath.Join(sysClassInfiniband, entry.Name(), "device")
		resolved, err := filepath.EvalSymlinks(devLink)
		if err != nil {
			continue
		}
		if filepath.Base(resolved) == pciAddr {
			return entry.Name(), nil
		}
	}
	return "", fmt.Errorf("no IB device found for PCI address %s", pciAddr)
}

// findNetDevices returns network interface names associated with an IB device.
func findNetDevices(ibDevName string) []string {
	netPath := filepath.Join(sysClassInfiniband, ibDevName, "device", "net")
	entries, err := os.ReadDir(netPath)
	if err != nil {
		return nil
	}

	var netDevs []string
	for _, entry := range entries {
		netDevs = append(netDevs, entry.Name())
	}
	return netDevs
}

func isPF(pciPath string) bool {
	// A PF has sriov_totalvfs file
	_, err := os.Stat(filepath.Join(pciPath, "sriov_totalvfs"))
	return err == nil
}

func isVF(pciPath string) bool {
	// A VF has a physfn symlink
	_, err := os.Lstat(filepath.Join(pciPath, "physfn"))
	return err == nil
}

func readIntFile(path string, defaultVal int) int {
	val, err := readIntFileErr(path)
	if err != nil {
		return defaultVal
	}
	return val
}

func readIntFileErr(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func readStringFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
