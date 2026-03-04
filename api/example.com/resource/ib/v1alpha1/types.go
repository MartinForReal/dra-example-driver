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

import "fmt"

// IbMTU represents valid InfiniBand MTU values.
type IbMTU int

const (
	MTU256  IbMTU = 256
	MTU512  IbMTU = 512
	MTU1024 IbMTU = 1024
	MTU2048 IbMTU = 2048
	MTU4096 IbMTU = 4096
)

// Validate ensures IbMTU has a valid value.
func (m IbMTU) Validate() error {
	switch m {
	case MTU256, MTU512, MTU1024, MTU2048, MTU4096:
		return nil
	}
	return fmt.Errorf("invalid IB MTU value: %d, must be one of 256, 512, 1024, 2048, 4096", m)
}
