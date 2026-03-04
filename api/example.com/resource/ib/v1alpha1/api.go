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

package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const IbConfigKind = "IbConfig"

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// IbConfig holds the set of parameters for configuring an InfiniBand device.
type IbConfig struct {
	metav1.TypeMeta `json:",inline"`

	// Pkey is the InfiniBand partition key (P_Key) for network isolation.
	// Valid range is 0x0001-0xFFFF. If nil, the fabric default (0xFFFF, full membership) is used.
	Pkey *uint16 `json:"pkey,omitempty"`

	// TrafficClass specifies the QoS traffic class for IB packets.
	// Valid range is 0-255. If nil, the default traffic class (0) is used.
	TrafficClass *uint8 `json:"trafficClass,omitempty"`

	// MTU specifies the Maximum Transmission Unit for the IB port.
	// Valid values are 256, 512, 1024, 2048, 4096. If nil, the port's active MTU is used.
	MTU *IbMTU `json:"mtu,omitempty"`
}

// DefaultIbConfig returns the default IB configuration with fabric defaults.
func DefaultIbConfig() *IbConfig {
	return &IbConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       IbConfigKind,
		},
		// nil fields = use fabric/port defaults
	}
}

// Normalize updates an IbConfig with implied default values based on other settings.
func (c *IbConfig) Normalize() error {
	if c == nil {
		return fmt.Errorf("config is 'nil'")
	}
	// All fields are optional; nil means "use default". Nothing to normalize.
	return nil
}
