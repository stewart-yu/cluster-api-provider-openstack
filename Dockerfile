# Copyright 2019 The Kubernetes Authors.
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

# Build the manager binary
#FROM golang:1.12.9 as builder
#
## Copy in the go src
#WORKDIR ${GOPATH}/src/sigs.k8s.io/cluster-api-provider-openstack
#COPY pkg/    pkg/
#COPY cmd/    cmd/
#COPY vendor/ vendor/
#COPY api/ api/
#COPY controllers/ controllers/
#COPY main.go main.go
#COPY go.mod go.mod
#COPY go.sum go.sum
#
## Build
#RUN CGO_ENABLED=0 GOOS=linux GO111MODULE=on GOFLAGS="-mod=vendor" \
#    go build -a -ldflags '-extldflags "-static"' \
#    -o manager sigs.k8s.io/cluster-api-provider-openstack
#
## Copy the controller-manager into a thin image
#FROM gcr.io/distroless/static:latest
#WORKDIR /
#COPY --from=builder /go/src/sigs.k8s.io/cluster-api-provider-openstack/manager .
#USER nobody
#ENTRYPOINT ["/manager"]

# Build the manager binary
FROM golang:1.12.9

# default the go proxy
ARG goproxy=https://proxy.golang.org

# run this with docker build --build_arg $(go env GOPROXY) to override the goproxy
ENV GOPROXY=$goproxy

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY pkg/    pkg/

# Allow containerd to restart pods by calling /restart.sh (mostly for tilt + fast dev cycles)
# TODO: Remove this on prod and use a multi-stage build
COPY third_party/forked/rerun-process-wrapper/start.sh .
COPY third_party/forked/rerun-process-wrapper/restart.sh .

# Build and run
RUN go install -v .
RUN mv /go/bin/cluster-api-provider-openstack /manager
ENTRYPOINT ["./start.sh", "/manager"]
