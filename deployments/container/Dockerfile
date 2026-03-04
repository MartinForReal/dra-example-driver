# Copyright 2023 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG GOLANG_VERSION=undefined
ARG BASE_IMAGE=undefined
FROM golang:${GOLANG_VERSION} as build

# Install libibverbs-dev for cgo compilation
RUN apt-get update && apt-get install -y --no-install-recommends \
    libibverbs-dev \
    librdmacm-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
COPY . .

RUN mkdir /artifacts
RUN CGO_ENABLED=1 make PREFIX=/artifacts cmds

FROM ${BASE_IMAGE}

# Install runtime dependencies for RDMA and network namespace management
RUN apt-get update && apt-get install -y --no-install-recommends \
    libibverbs1 \
    librdmacm1 \
    rdma-core \
    iproute2 \
    && rm -rf /var/lib/apt/lists/*

LABEL io.k8s.display-name="InfiniBand DRA Resource Driver for Dynamic Resource Allocation"
LABEL name="InfiniBand DRA Resource Driver for Dynamic Resource Allocation"
LABEL vendor="sigs.k8s.io"
ARG VERSION="N/A"
LABEL version=${VERSION}
ARG RELEASE="N/A"
LABEL release="N/A"
LABEL summary="InfiniBand DRA resource driver for Kubernetes"
LABEL description="See summary"

COPY --from=build /artifacts/dra-example-kubeletplugin /usr/bin/dra-example-kubeletplugin
COPY --from=build /artifacts/dra-example-webhook       /usr/bin/dra-example-webhook
