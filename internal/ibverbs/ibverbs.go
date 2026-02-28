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

// Package ibverbs provides Go bindings to libibverbs for InfiniBand device
// discovery and attribute querying via cgo.
package ibverbs

/*
#cgo LDFLAGS: -libverbs
#include <infiniband/verbs.h>
#include <stdlib.h>
#include <string.h>

// Helper to get device list
static struct ibv_device **get_device_list(int *num) {
    return ibv_get_device_list(num);
}

// Helper to free device list
static void put_device_list(struct ibv_device **list) {
    ibv_free_device_list(list);
}

// Wrapper for ibv_query_port to work around struct compatibility issues
// in newer rdma-core versions.
static int query_port_compat(struct ibv_context *ctx, uint8_t port_num,
                             enum ibv_port_state *state,
                             int *active_mtu,
                             int *active_speed,
                             int *active_width,
                             uint16_t *lid,
                             uint8_t *link_layer) {
    struct ibv_port_attr attr;
    memset(&attr, 0, sizeof(attr));
    int rc = ibv_query_port(ctx, port_num, &attr);
    if (rc == 0) {
        *state = attr.state;
        *active_mtu = attr.active_mtu;
        *active_speed = attr.active_speed;
        *active_width = attr.active_width;
        *lid = attr.lid;
        *link_layer = attr.link_layer;
    }
    return rc;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// PortState represents the state of an IB port.
type PortState int

const (
	PortStateNOP    PortState = 0
	PortStateDown   PortState = 1
	PortStateInit   PortState = 2
	PortStateArmed  PortState = 3
	PortStateActive PortState = 4
)

func (s PortState) String() string {
	switch s {
	case PortStateDown:
		return "Down"
	case PortStateInit:
		return "Init"
	case PortStateArmed:
		return "Armed"
	case PortStateActive:
		return "Active"
	default:
		return "Unknown"
	}
}

// LinkSpeed represents the link speed.
type LinkSpeed int

func (s LinkSpeed) String() string {
	// Map active_speed values to human-readable strings.
	// These correspond to IB spec speed values.
	switch s {
	case 1:
		return "2.5Gb/s"
	case 2:
		return "5Gb/s"
	case 4, 8:
		return "10Gb/s"
	case 16:
		return "14Gb/s"
	case 32:
		return "25Gb/s"
	case 64:
		return "50Gb/s"
	case 128:
		return "100Gb/s"
	case 256:
		return "200Gb/s"
	case 512:
		return "400Gb/s"
	default:
		return fmt.Sprintf("%dGb/s", s)
	}
}

// PortInfo holds information about an IB port.
type PortInfo struct {
	PortNum     int
	State       PortState
	ActiveSpeed LinkSpeed
	ActiveWidth int
	LID         uint16
	ActiveMTU   int
	GID         [16]byte
	LinkLayer   string
}

// EffectiveSpeed returns the effective port speed in Gb/s taking width into account.
func (p *PortInfo) EffectiveSpeed() string {
	// Width mapping: 1=1x, 2=4x, 4=8x, 8=12x
	widthMultiplier := 1
	switch p.ActiveWidth {
	case 1:
		widthMultiplier = 1
	case 2:
		widthMultiplier = 4
	case 4:
		widthMultiplier = 8
	case 8:
		widthMultiplier = 12
	}

	// Get base speed in Gb/s
	baseSpeed := 0
	switch int(p.ActiveSpeed) {
	case 1:
		baseSpeed = 2 // SDR: 2.5 Gb/s
	case 2:
		baseSpeed = 5 // DDR: 5 Gb/s
	case 4:
		baseSpeed = 10 // QDR: 10 Gb/s
	case 8:
		baseSpeed = 10 // FDR10: 10 Gb/s
	case 16:
		baseSpeed = 14 // FDR: 14 Gb/s
	case 32:
		baseSpeed = 25 // EDR: 25 Gb/s
	case 64:
		baseSpeed = 50 // HDR: 50 Gb/s
	case 128:
		baseSpeed = 100 // NDR: 100 Gb/s
	case 256:
		baseSpeed = 200 // XDR: 200 Gb/s
	default:
		return fmt.Sprintf("%dGb/s", int(p.ActiveSpeed)*widthMultiplier)
	}

	totalSpeed := baseSpeed * widthMultiplier
	return fmt.Sprintf("%dGb/s", totalSpeed)
}

// DeviceInfo holds information about a single IB device.
type DeviceInfo struct {
	Name            string
	NodeGUID        uint64
	FirmwareVersion string
	NumPorts        int
	Ports           []PortInfo
	NodeType        int // IBV_NODE_CA, IBV_NODE_SWITCH, etc.
	TransportType   int
	VendorID        uint32
	DeviceID        uint32
}

// NodeGUIDString returns the node GUID as a formatted string.
func (d *DeviceInfo) NodeGUIDString() string {
	return fmt.Sprintf("%016x", d.NodeGUID)
}

// ListDevices enumerates all InfiniBand devices on the host using libibverbs.
func ListDevices() ([]DeviceInfo, error) {
	var numDevices C.int
	devList := C.get_device_list(&numDevices)
	if devList == nil {
		return nil, fmt.Errorf("ibv_get_device_list failed")
	}
	defer C.put_device_list(devList)

	if int(numDevices) == 0 {
		return nil, nil
	}

	// Convert the C array to a Go slice of pointers
	devSlice := unsafe.Slice(devList, int(numDevices))

	var devices []DeviceInfo
	for i := 0; i < int(numDevices); i++ {
		dev := devSlice[i]
		if dev == nil {
			continue
		}

		devInfo, err := queryDevice(dev)
		if err != nil {
			// Log and continue, don't fail on a single device
			continue
		}
		devices = append(devices, *devInfo)
	}

	return devices, nil
}

func queryDevice(dev *C.struct_ibv_device) (*DeviceInfo, error) {
	ctx := C.ibv_open_device(dev)
	if ctx == nil {
		return nil, fmt.Errorf("ibv_open_device failed for %s", C.GoString(C.ibv_get_device_name(dev)))
	}
	defer C.ibv_close_device(ctx)

	name := C.GoString(C.ibv_get_device_name(dev))

	var deviceAttr C.struct_ibv_device_attr
	if rc := C.ibv_query_device(ctx, &deviceAttr); rc != 0 {
		return nil, fmt.Errorf("ibv_query_device failed for %s: %d", name, rc)
	}

	info := &DeviceInfo{
		Name:            name,
		NodeGUID:        uint64(C.ibv_get_device_guid(dev)),
		FirmwareVersion: C.GoString(&deviceAttr.fw_ver[0]),
		NumPorts:        int(deviceAttr.phys_port_cnt),
		VendorID:        uint32(deviceAttr.vendor_id),
		DeviceID:        uint32(deviceAttr.vendor_part_id),
	}

	info.NodeType = int(dev.node_type)
	info.TransportType = int(dev.transport_type)

	// Query each port
	for port := 1; port <= info.NumPorts; port++ {
		portInfo, err := queryPort(ctx, port)
		if err != nil {
			continue
		}
		info.Ports = append(info.Ports, *portInfo)
	}

	return info, nil
}

func queryPort(ctx *C.struct_ibv_context, portNum int) (*PortInfo, error) {
	var (
		state       C.enum_ibv_port_state
		activeMTU   C.int
		activeSpeed C.int
		activeWidth C.int
		lid         C.uint16_t
		linkLayer   C.uint8_t
	)

	rc := C.query_port_compat(ctx, C.uint8_t(portNum),
		&state, &activeMTU, &activeSpeed, &activeWidth, &lid, &linkLayer)
	if rc != 0 {
		return nil, fmt.Errorf("ibv_query_port failed for port %d: %d", portNum, rc)
	}

	pi := &PortInfo{
		PortNum:     portNum,
		State:       PortState(state),
		ActiveSpeed: LinkSpeed(activeSpeed),
		ActiveWidth: int(activeWidth),
		LID:         uint16(lid),
		ActiveMTU:   mtuEnumToBytes(int(activeMTU)),
	}

	// Determine link layer
	switch linkLayer {
	case C.IBV_LINK_LAYER_INFINIBAND:
		pi.LinkLayer = "InfiniBand"
	case C.IBV_LINK_LAYER_ETHERNET:
		pi.LinkLayer = "Ethernet"
	default:
		pi.LinkLayer = "Unknown"
	}

	// Query GID at index 0
	var gid C.union_ibv_gid
	if rc := C.ibv_query_gid(ctx, C.uint8_t(portNum), 0, &gid); rc == 0 {
		raw := (*[16]byte)(unsafe.Pointer(&gid))
		copy(pi.GID[:], raw[:])
	}

	return pi, nil
}

// mtuEnumToBytes converts IBV MTU enum to byte value.
func mtuEnumToBytes(mtuEnum int) int {
	switch mtuEnum {
	case 1:
		return 256
	case 2:
		return 512
	case 3:
		return 1024
	case 4:
		return 2048
	case 5:
		return 4096
	default:
		return 0
	}
}
