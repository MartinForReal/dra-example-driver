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

// Package sriov provides SR-IOV Virtual Function lifecycle management for
// InfiniBand devices on baremetal hosts.
package sriov

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-example-driver/internal/sysfs"
)

const (
	// vfSettleTimeout is how long to wait for VFs to appear in sysfs after creation.
	vfSettleTimeout = 30 * time.Second
	// vfPollInterval is how frequently to poll for VF appearance.
	vfPollInterval = 500 * time.Millisecond
)

// PFInfo holds information about a Physical Function that supports SR-IOV.
type PFInfo struct {
	PCIAddress string
	IBDevName  string
	TotalVFs   int
	CurrentVFs int
}

// DiscoverSRIOVPFs finds all IB PFs on the host that support SR-IOV.
func DiscoverSRIOVPFs() ([]PFInfo, error) {
	ibDevices, err := sysfs.ListIBDevices()
	if err != nil {
		return nil, fmt.Errorf("list IB devices: %w", err)
	}

	var pfs []PFInfo
	for _, dev := range ibDevices {
		if !dev.IsPF || dev.SRIOVTotalVFs == 0 {
			continue
		}
		pfs = append(pfs, PFInfo{
			PCIAddress: dev.PCIAddress,
			IBDevName:  dev.Name,
			TotalVFs:   dev.SRIOVTotalVFs,
			CurrentVFs: dev.SRIOVNumVFs,
		})
	}
	return pfs, nil
}

// ProvisionVFs ensures that at least `desired` VFs exist for the PF at the
// given PCI address. If VFs already exist but fewer than `desired`, the current
// VFs are first removed and then re-created with the desired count (SR-IOV
// sysfs requires writing 0 before changing the count).
//
// This is a startup-time operation: the pool of VFs is pre-created and then
// treated as a fixed inventory.
func ProvisionVFs(ctx context.Context, pfPCIAddr string, desired int) error {
	logger := klog.FromContext(ctx)

	totalVFs, err := sysfs.GetSRIOVTotalVFs(pfPCIAddr)
	if err != nil {
		return fmt.Errorf("get sriov_totalvfs for %s: %w", pfPCIAddr, err)
	}

	if desired > totalVFs {
		return fmt.Errorf("requested %d VFs exceeds maximum %d for PF %s", desired, totalVFs, pfPCIAddr)
	}

	currentVFs, err := sysfs.GetSRIOVNumVFs(pfPCIAddr)
	if err != nil {
		return fmt.Errorf("get sriov_numvfs for %s: %w", pfPCIAddr, err)
	}

	if currentVFs == desired {
		logger.V(2).Info("VFs already at desired count", "pf", pfPCIAddr, "count", desired)
		return nil
	}

	// Must reset to 0 before changing
	if currentVFs > 0 {
		logger.Info("Resetting existing VFs before reprovisioning", "pf", pfPCIAddr, "current", currentVFs, "desired", desired)
		if err := sysfs.SetSRIOVNumVFs(pfPCIAddr, 0); err != nil {
			return fmt.Errorf("reset sriov_numvfs to 0 for %s: %w", pfPCIAddr, err)
		}
		// Brief pause after destroying VFs
		time.Sleep(1 * time.Second)
	}

	logger.Info("Creating VFs", "pf", pfPCIAddr, "count", desired)
	if err := sysfs.SetSRIOVNumVFs(pfPCIAddr, desired); err != nil {
		return fmt.Errorf("set sriov_numvfs to %d for %s: %w", desired, pfPCIAddr, err)
	}

	// Wait for VFs to appear
	if err := waitForVFs(pfPCIAddr, desired); err != nil {
		return fmt.Errorf("VFs did not appear for %s: %w", pfPCIAddr, err)
	}

	logger.Info("VFs provisioned successfully", "pf", pfPCIAddr, "count", desired)
	return nil
}

// DestroyVFs removes all VFs for the given PF.
func DestroyVFs(pfPCIAddr string) error {
	return sysfs.SetSRIOVNumVFs(pfPCIAddr, 0)
}

// GetVFPCIAddresses returns the PCI addresses of all VFs belonging to the PF.
func GetVFPCIAddresses(pfPCIAddr string) ([]string, error) {
	return sysfs.ListVFs(pfPCIAddr)
}

// waitForVFs polls sysfs until the expected number of VFs appear or a timeout is reached.
func waitForVFs(pfPCIAddr string, expected int) error {
	deadline := time.Now().Add(vfSettleTimeout)
	for time.Now().Before(deadline) {
		vfs, err := sysfs.ListVFs(pfPCIAddr)
		if err == nil && len(vfs) >= expected {
			return nil
		}
		time.Sleep(vfPollInterval)
	}
	return fmt.Errorf("timed out waiting for %d VFs on PF %s", expected, pfPCIAddr)
}
