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
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"
)

func TestValidateIbConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  *IbConfig
		wantErr bool
	}{
		{
			name:    "default config (all nil) is valid",
			config:  DefaultIbConfig(),
			wantErr: false,
		},
		{
			name: "valid pkey",
			config: &IbConfig{
				Pkey: ptr.To(uint16(0x8001)),
			},
			wantErr: false,
		},
		{
			name: "pkey 0x0000 is invalid",
			config: &IbConfig{
				Pkey: ptr.To(uint16(0)),
			},
			wantErr: true,
		},
		{
			name: "full membership pkey is valid",
			config: &IbConfig{
				Pkey: ptr.To(uint16(0xFFFF)),
			},
			wantErr: false,
		},
		{
			name: "valid MTU 4096",
			config: &IbConfig{
				MTU: ptr.To(MTU4096),
			},
			wantErr: false,
		},
		{
			name: "invalid MTU",
			config: &IbConfig{
				MTU: ptr.To(IbMTU(9000)),
			},
			wantErr: true,
		},
		{
			name: "valid traffic class",
			config: &IbConfig{
				TrafficClass: ptr.To(uint8(128)),
			},
			wantErr: false,
		},
		{
			name: "all fields set and valid",
			config: &IbConfig{
				Pkey:         ptr.To(uint16(0x8001)),
				TrafficClass: ptr.To(uint8(64)),
				MTU:          ptr.To(MTU2048),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestIbMTUValidate(t *testing.T) {
	validMTUs := []IbMTU{MTU256, MTU512, MTU1024, MTU2048, MTU4096}
	for _, mtu := range validMTUs {
		assert.NoError(t, mtu.Validate(), "MTU %d should be valid", mtu)
	}

	invalidMTUs := []IbMTU{0, 128, 9000, 3000}
	for _, mtu := range invalidMTUs {
		assert.Error(t, mtu.Validate(), "MTU %d should be invalid", mtu)
	}
}
