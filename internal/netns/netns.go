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

// Package netns provides helpers for moving InfiniBand network devices and
// RDMA devices between Linux network namespaces. This is used to isolate
// IB devices for containers.
package netns

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

// MoveNetdevToContainerNetns moves a network device into a container's network
// namespace identified by the container's PID. This is typically called from
// a CDI createRuntime hook.
func MoveNetdevToContainerNetns(ctx context.Context, netdev string, containerPID int) error {
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Moving netdev to container netns", "netdev", netdev, "pid", containerPID)

	// ip link set <netdev> netns <pid>
	cmd := exec.Command("ip", "link", "set", netdev, "netns", strconv.Itoa(containerPID))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("move netdev %s to netns of pid %d: %w (output: %s)", netdev, containerPID, err, strings.TrimSpace(string(output)))
	}

	// Bring up the interface inside the container netns
	cmd = exec.Command("nsenter", "-t", strconv.Itoa(containerPID), "-n", "--",
		"ip", "link", "set", netdev, "up")
	output, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bring up netdev %s in container netns: %w (output: %s)", netdev, err, strings.TrimSpace(string(output)))
	}

	return nil
}

// MoveNetdevToHostNetns moves a network device back to the host (init) network
// namespace. This is called during device unprepare / cleanup.
func MoveNetdevToHostNetns(ctx context.Context, netdev string, containerPID int) error {
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Moving netdev back to host netns", "netdev", netdev, "pid", containerPID)

	// nsenter into container netns, then move device to PID 1's netns (host)
	cmd := exec.Command("nsenter", "-t", strconv.Itoa(containerPID), "-n", "--",
		"ip", "link", "set", netdev, "netns", "1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("move netdev %s back to host netns: %w (output: %s)", netdev, err, strings.TrimSpace(string(output)))
	}

	return nil
}

// MoveRDMADevToContainerNetns moves an RDMA device into a container's network
// namespace. Requires the host RDMA subsystem to be in "exclusive" netns mode
// (rdma system set netns exclusive).
func MoveRDMADevToContainerNetns(ctx context.Context, rdmaDev string, containerPID int) error {
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Moving RDMA device to container netns", "rdmaDev", rdmaDev, "pid", containerPID)

	// rdma dev set <rdmaDev> netns <pid>
	cmd := exec.Command("rdma", "dev", "set", rdmaDev, "netns", strconv.Itoa(containerPID))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("move RDMA device %s to netns of pid %d: %w (output: %s)", rdmaDev, containerPID, err, strings.TrimSpace(string(output)))
	}

	return nil
}

// EnsureRDMAExclusiveMode sets the RDMA subsystem to exclusive network
// namespace mode. In this mode, RDMA devices are isolated per-netns.
func EnsureRDMAExclusiveMode(ctx context.Context) error {
	logger := klog.FromContext(ctx)

	// Check current mode
	cmd := exec.Command("rdma", "system")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("query rdma system mode: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	if strings.Contains(string(output), "exclusive") {
		logger.V(2).Info("RDMA subsystem already in exclusive netns mode")
		return nil
	}

	logger.Info("Setting RDMA subsystem to exclusive netns mode")
	cmd = exec.Command("rdma", "system", "set", "netns", "exclusive")
	output, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("set rdma netns exclusive: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	return nil
}

// MoveNetdevToNetns moves a network device into a network namespace identified
// by its filesystem path (e.g. /proc/<pid>/ns/net or /var/run/netns/<name>).
// This is used by NRI hooks which have direct access to the namespace path.
func MoveNetdevToNetns(ctx context.Context, netdev string, nsPath string) error {
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Moving netdev to namespace path", "netdev", netdev, "nsPath", nsPath)

	// ip link set <netdev> netns <nsPath>
	// Note: "ip link set ... netns" accepts either a PID or a netns name.
	// For a path, we use nsenter to run the ip command.
	cmd := exec.Command("nsenter", "--net="+nsPath, "--", "ip", "link", "list")
	if _, err := cmd.CombinedOutput(); err != nil {
		// Namespace doesn't exist or isn't accessible
		return fmt.Errorf("check netns %s: %w", nsPath, err)
	}

	// Move the device. We use the "ip link set" approach with PID extracted from path
	// or use nsenter-based method.
	cmd = exec.Command("ip", "link", "set", netdev, "netns", nsPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("move netdev %s to netns %s: %w (output: %s)", netdev, nsPath, err, strings.TrimSpace(string(output)))
	}

	// Bring up the interface inside the namespace
	cmd = exec.Command("nsenter", "--net="+nsPath, "--",
		"ip", "link", "set", netdev, "up")
	output, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bring up netdev %s in netns %s: %w (output: %s)", netdev, nsPath, err, strings.TrimSpace(string(output)))
	}

	return nil
}

// MoveNetdevBackToHost moves a network device from a namespace back to the
// host (init) network namespace. The nsPath is the filesystem path of the
// network namespace the device currently lives in.
func MoveNetdevBackToHost(ctx context.Context, netdev string, nsPath string) error {
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Moving netdev back to host netns", "netdev", netdev, "nsPath", nsPath)

	cmd := exec.Command("nsenter", "--net="+nsPath, "--",
		"ip", "link", "set", netdev, "netns", "1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("move netdev %s back to host netns: %w (output: %s)", netdev, err, strings.TrimSpace(string(output)))
	}

	return nil
}

// MoveRDMADevToNetns moves an RDMA device to a network namespace identified
// by path. Requires the host RDMA subsystem to be in "exclusive" netns mode.
func MoveRDMADevToNetns(ctx context.Context, rdmaDev string, nsPath string) error {
	logger := klog.FromContext(ctx)
	logger.V(2).Info("Moving RDMA device to netns", "rdmaDev", rdmaDev, "nsPath", nsPath)

	// Extract PID from nsPath if it's /proc/<pid>/ns/net
	parts := strings.Split(nsPath, "/")
	pid := ""
	for i, p := range parts {
		if p == "proc" && i+2 < len(parts) && parts[i+2] == "ns" {
			pid = parts[i+1]
			break
		}
	}

	if pid != "" {
		cmd := exec.Command("rdma", "dev", "set", rdmaDev, "netns", pid)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("move RDMA dev %s to netns pid %s: %w (output: %s)", rdmaDev, pid, err, strings.TrimSpace(string(output)))
		}
		return nil
	}

	// If we can't extract PID, try using nsenter to run rdma command
	cmd := exec.Command("nsenter", "--net="+nsPath, "--",
		"rdma", "dev")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("RDMA dev query in netns %s: %w (output: %s)", nsPath, err, strings.TrimSpace(string(output)))
	}
	logger.V(4).Info("RDMA devices in namespace", "output", string(output))

	return nil
}

// GenerateMoveNetdevCommand returns the command and args that should be used
// as a CDI hook to move a network device into a container's namespace.
// The pluginBinary is the path to the DRA plugin binary which is re-invoked
// as a helper.
func GenerateMoveNetdevCommand(pluginBinary, netdev, rdmaDev string) (string, []string) {
	args := []string{
		"move-netdev",
		"--netdev", netdev,
	}
	if rdmaDev != "" {
		args = append(args, "--rdma-dev", rdmaDev)
	}
	return pluginBinary, args
}
