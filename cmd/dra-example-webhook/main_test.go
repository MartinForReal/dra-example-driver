/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	resourceapi "k8s.io/api/resource/v1"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	configapi "sigs.k8s.io/dra-example-driver/api/example.com/resource/ib/v1alpha1"
	"sigs.k8s.io/dra-example-driver/internal/profiles/ib"
)

const driverName = "ib.sigs.k8s.io"

func TestReadyEndpoint(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(readyHandler))
	t.Cleanup(s.Close)

	res, err := http.Get(s.URL)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, res.StatusCode)
}

func TestResourceClaimValidatingWebhook(t *testing.T) {
	unknownResource := metav1.GroupVersionResource{
		Group:    "resource.k8s.io",
		Version:  "v1",
		Resource: "unknownresources",
	}

	validIbConfig := &configapi.IbConfig{
		Pkey:         ptr.To(uint16(0x8001)),
		TrafficClass: ptr.To(uint8(0)),
		MTU:          ptr.To(configapi.MTU4096),
	}

	invalidIbConfigs := []*configapi.IbConfig{
		{
			Pkey: ptr.To(uint16(0)),
		},
		{
			MTU: ptr.To(configapi.IbMTU(9999)),
		},
	}

	tests := map[string]struct {
		admissionReview      *admissionv1.AdmissionReview
		requestContentType   string
		expectedResponseCode int
		expectedAllowed      bool
		expectedMessage      string
	}{
		"bad contentType": {
			requestContentType:   "invalid type",
			expectedResponseCode: http.StatusUnsupportedMediaType,
		},
		"invalid AdmissionReview": {
			admissionReview:      &admissionv1.AdmissionReview{},
			expectedResponseCode: http.StatusBadRequest,
		},
		"valid IbConfig in ResourceClaim": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithIbConfigs(validIbConfig),
				resourceClaimResourceV1,
			),
			expectedAllowed: true,
		},
		"invalid IbConfigs in ResourceClaim": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithIbConfigs(invalidIbConfigs...),
				resourceClaimResourceV1,
			),
			expectedAllowed: false,
			expectedMessage: "2 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: invalid IbConfig: pkey must be in range 0x0001-0xFFFF, got 0x0000; object at spec.devices.config[1].opaque.parameters is invalid: invalid IbConfig: invalid IB MTU value: 9999, must be one of 256, 512, 1024, 2048, 4096",
		},
		"valid IbConfig in ResourceClaimTemplate": {
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithIbConfigs(validIbConfig),
				resourceClaimTemplateResourceV1,
			),
			expectedAllowed: true,
		},
		"invalid IbConfigs in ResourceClaimTemplate": {
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithIbConfigs(invalidIbConfigs...),
				resourceClaimTemplateResourceV1,
			),
			expectedAllowed: false,
			expectedMessage: "2 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: invalid IbConfig: pkey must be in range 0x0001-0xFFFF, got 0x0000; object at spec.spec.devices.config[1].opaque.parameters is invalid: invalid IbConfig: invalid IB MTU value: 9999, must be one of 256, 512, 1024, 2048, 4096",
		},
		"valid IbConfig in ResourceClaim v1beta1": {
			admissionReview: admissionReviewWithObject(
				toResourceClaimV1Beta1(resourceClaimWithIbConfigs(validIbConfig)),
				resourceClaimResourceV1Beta1,
			),
			expectedAllowed: true,
		},
		"invalid IbConfigs in ResourceClaim v1beta1": {
			admissionReview: admissionReviewWithObject(
				toResourceClaimV1Beta1(resourceClaimWithIbConfigs(invalidIbConfigs...)),
				resourceClaimResourceV1Beta1,
			),
			expectedAllowed: false,
			expectedMessage: "2 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: invalid IbConfig: pkey must be in range 0x0001-0xFFFF, got 0x0000; object at spec.devices.config[1].opaque.parameters is invalid: invalid IbConfig: invalid IB MTU value: 9999, must be one of 256, 512, 1024, 2048, 4096",
		},
		"valid IbConfig in ResourceClaimTemplate v1beta1": {
			admissionReview: admissionReviewWithObject(
				toResourceClaimTemplateV1Beta1(resourceClaimTemplateWithIbConfigs(validIbConfig)),
				resourceClaimTemplateResourceV1Beta1,
			),
			expectedAllowed: true,
		},
		"invalid IbConfigs in ResourceClaimTemplate v1beta1": {
			admissionReview: admissionReviewWithObject(
				toResourceClaimTemplateV1Beta1(resourceClaimTemplateWithIbConfigs(invalidIbConfigs...)),
				resourceClaimTemplateResourceV1Beta1,
			),
			expectedAllowed: false,
			expectedMessage: "2 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: invalid IbConfig: pkey must be in range 0x0001-0xFFFF, got 0x0000; object at spec.spec.devices.config[1].opaque.parameters is invalid: invalid IbConfig: invalid IB MTU value: 9999, must be one of 256, 512, 1024, 2048, 4096",
		},
		"unknown resource type": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithIbConfigs(validIbConfig),
				unknownResource,
			),
			expectedAllowed: false,
			expectedMessage: "expected resource to be one of [{resource.k8s.io v1 resourceclaims} {resource.k8s.io v1beta1 resourceclaims} {resource.k8s.io v1beta2 resourceclaims} {resource.k8s.io v1 resourceclaimtemplates} {resource.k8s.io v1beta1 resourceclaimtemplates} {resource.k8s.io v1beta2 resourceclaimtemplates}], got {resource.k8s.io v1 unknownresources}",
		},
	}

	configHandler := ib.Profile{}
	mux, err := newMux(configHandler, driverName)
	assert.NoError(t, err)

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			requestBody, err := json.Marshal(test.admissionReview)
			require.NoError(t, err)

			contentType := test.requestContentType
			if contentType == "" {
				contentType = "application/json"
			}

			res, err := http.Post(s.URL+"/validate-resource-claim-parameters", contentType, bytes.NewReader(requestBody))
			require.NoError(t, err)
			expectedResponseCode := test.expectedResponseCode
			if expectedResponseCode == 0 {
				expectedResponseCode = http.StatusOK
			}
			assert.Equal(t, expectedResponseCode, res.StatusCode)
			if res.StatusCode != http.StatusOK {
				// We don't have an AdmissionReview to validate
				return
			}

			responseBody, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			res.Body.Close()

			responseAdmissionReview, err := readAdmissionReview(responseBody)
			assert.NoError(t, err)
			assert.Equal(t, test.expectedAllowed, responseAdmissionReview.Response.Allowed)
			if !test.expectedAllowed {
				assert.Equal(t, test.expectedMessage, string(responseAdmissionReview.Response.Result.Message))
			}
		})
	}
}

func admissionReviewWithObject(obj runtime.Object, resource metav1.GroupVersionResource) *admissionv1.AdmissionReview {
	requestedAdmissionReview := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			Resource: resource,
			Object: runtime.RawExtension{
				Object: obj,
			},
		},
	}
	requestedAdmissionReview.SetGroupVersionKind(admissionv1.SchemeGroupVersion.WithKind("AdmissionReview"))
	return requestedAdmissionReview
}

func resourceClaimWithIbConfigs(ibConfigs ...*configapi.IbConfig) *resourceapi.ResourceClaim {
	resourceClaim := &resourceapi.ResourceClaim{
		Spec: resourceClaimSpecWithIbConfigs(ibConfigs...),
	}
	resourceClaim.SetGroupVersionKind(resourceapi.SchemeGroupVersion.WithKind("ResourceClaim"))
	return resourceClaim
}

func resourceClaimTemplateWithIbConfigs(ibConfigs ...*configapi.IbConfig) *resourceapi.ResourceClaimTemplate {
	resourceClaimTemplate := &resourceapi.ResourceClaimTemplate{
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceClaimSpecWithIbConfigs(ibConfigs...),
		},
	}
	resourceClaimTemplate.SetGroupVersionKind(resourceapi.SchemeGroupVersion.WithKind("ResourceClaimTemplate"))
	return resourceClaimTemplate
}

func resourceClaimSpecWithIbConfigs(ibConfigs ...*configapi.IbConfig) resourceapi.ResourceClaimSpec {
	resourceClaimSpec := resourceapi.ResourceClaimSpec{}
	for _, ibConfig := range ibConfigs {
		ibConfig.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   configapi.GroupName,
			Version: configapi.Version,
			Kind:    "IbConfig",
		})
		deviceConfig := resourceapi.DeviceClaimConfiguration{
			DeviceConfiguration: resourceapi.DeviceConfiguration{
				Opaque: &resourceapi.OpaqueDeviceConfiguration{
					Driver: driverName,
					Parameters: runtime.RawExtension{
						Object: ibConfig,
					},
				},
			},
		}
		resourceClaimSpec.Devices.Config = append(resourceClaimSpec.Devices.Config, deviceConfig)
	}
	return resourceClaimSpec
}

func toResourceClaimV1Beta1(v1Claim *resourceapi.ResourceClaim) *resourcev1beta1.ResourceClaim {
	v1beta1Claim := &resourcev1beta1.ResourceClaim{}
	if err := scheme.Convert(v1Claim, v1beta1Claim, nil); err != nil {
		panic(fmt.Sprintf("failed to convert ResourceClaim to v1beta1: %v", err))
	}
	return v1beta1Claim
}

func toResourceClaimTemplateV1Beta1(v1Template *resourceapi.ResourceClaimTemplate) *resourcev1beta1.ResourceClaimTemplate {
	v1beta1Template := &resourcev1beta1.ResourceClaimTemplate{}
	if err := scheme.Convert(v1Template, v1beta1Template, nil); err != nil {
		panic(fmt.Sprintf("failed to convert ResourceClaimTemplate to v1beta1: %v", err))
	}
	return v1beta1Template
}
