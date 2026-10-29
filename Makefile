# ------------------------------------------------------------------------------
# Makefile contains rules for building and testing the cockroach-operator.
# Docker Desktop must be running. See the README.md file for recommendations on
# how to use the commands.
# ------------------------------------------------------------------------------

# TOOLPATH is the location of tools used during builds.
TOOLPATH := $(abspath bin)

# We install our vendored tools to a directory within this repository to avoid
# overwriting any user-installed binaries of the same name in the default GOBIN.
GO_INSTALL := GOBIN='$(TOOLPATH)' GOFLAGS= GOTOOLCHAIN=go1.26.10 go install

# Use amd64 as the ARCH_NAME for x86_64. x86_64 is the name reported by the
# kernel on macos and linux. amd64 is the name used by GOARCH.
ARCH_NAME := $(shell uname -m | sed s/x86_64/amd64/)
OS_NAME   := $(shell uname -s | tr A-Z a-z)

K3D_VERSION := 5.8.3

# Prefer tools we've built or installed with Go to those elsewhere in the PATH.
export PATH := $(TOOLPATH):$(PATH)

# Colors for making more pleasing output.
CYAN := $(shell tput setaf 6)
NC := $(shell tput sgr0) # No Color

# Fetch dependencies with aggressive retry policies so that CI builds don't fail due to flaky downloads.
# TODO(dev-inf): Configure a caching proxy or similar.
WGET := wget --continue --tries 25 --read-timeout 30 --retry-connrefused --waitretry=5

bin/dl:
	mkdir -p ./bin/dl

# k3d is a tool for creating lightweight kubernetes clusters
bin/k3d: bin/dl/k3d-$(K3D_VERSION)-$(OS_NAME)-$(ARCH_NAME)
	ln -fs $(shell pwd)/$< $@

bin/dl/k3d-$(K3D_VERSION)-$(OS_NAME)-$(ARCH_NAME): bin/dl
	$(WGET) -O $@ https://github.com/rancher/k3d/releases/download/v$(K3D_VERSION)/k3d-$(OS_NAME)-$(ARCH_NAME)
	chmod a+x $@

.PHONY: bin/modernize
bin/modernize:
	$(GO_INSTALL) golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize@v0.44.0

# Version variables for stamping local go build binaries with version info.
# Bazel builds use workspace_status_command (build/stamp.sh) instead.
OPERATOR_VERSION ?= $(shell cat VERSION 2>/dev/null || echo "dev")
COMMIT_SHA       ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_TAG        ?= $(shell git describe --tags 2>/dev/null || echo "unknown")
BUILD_VERSION    ?= $(shell scripts/version.sh 2>/dev/null || echo "unknown")

BUILDINFO_PKG := github.com/cockroachlabs/cockroach-enterprise-operator/buildinfo
CONSTANTS_PKG := github.com/cockroachlabs/cockroach-enterprise-operator/pkg/constants

# typ and utcTime are intentionally omitted. Local builds are not releases, so
# IsRelease() returns false and ContainerImage() uses the default tag ("latest")
# rather than a version tag. Bazel CI builds stamp all fields via x_defs.
LDFLAGS := \
	-X "$(BUILDINFO_PKG).operatorVersion=$(OPERATOR_VERSION)" \
	-X "$(BUILDINFO_PKG).rev=$(COMMIT_SHA)" \
	-X "$(BUILDINFO_PKG).tag=$(BUILD_TAG)" \
	-X "$(BUILDINFO_PKG).version=$(BUILD_VERSION)" \
	-X "$(CONSTANTS_PKG).operatorVersion=$(OPERATOR_VERSION)"

# NOTE: Keep the "build" rule as the first rule so that "make" will invoke it
# when it has no arguments.
#
# build ensures that all generated code and configs are up-to-date and builds
# the cockroach-operator binary.
.PHONY: build
build: generate fmt
	@go build -ldflags '$(LDFLAGS)'

# local-cluster publishes the image to the local registry and starts up a k3d
# cluster with the operator deployed in the default namespace.
local-cluster: push/local
	@echo "$(CYAN)Deploying the operator to a local K3D cluster...$(NC)"
	@go test ./testenv -tags integration -debug-env -run TestIntegrationRun$$

# local-cluster publishes the image to the local registry and starts up a k3d
# cluster with the operator and a dedicated CrdbCluster deployed in the default
# namespace.
local-dedicated-cluster: push/local
	@echo "$(CYAN)Deploying the operator to a local K3D cluster...$(NC)"
	@echo "$(CYAN)Setting up a dedicated CrdbCluster in local K3D cluster...$(NC)"
	@go test ./testenv -tags integration -debug-env -run TestIntegrationRunDedicated$$

# local-serverless-cluster publishes the image to the local registry and starts
# up a k3d cluster with the operator and a serverless CrdbCluster deployed in
# the default namespace.
local-serverless-cluster: push/local
	@echo "$(CYAN)See LocalServerless.md for help using the local cluster.$(NC)"
	@echo "$(CYAN)Setting up Serverless in local K3D cluster...$(NC)"
	@go test ./testenv -tags integration -debug-env -run TestIntegrationRunServerless

# test runs all tests using the latest cockroach-operator manifest and binary.
# Many of the tests use kuberecord, which pre-records K8s API responses and then
# plays them back for fast, parallelizable tests. If the tests fail with error
# messages indicating they need to be re-recorded, use "make test/record".
.PHONY: test
test: generate
	go test ./...

# test/record re-records all failed tests that use the kuberecord package. This
# is slow, since it requires using real K8s clusters and it can't be
# parallelized.
.PHONY: test/record
test/record: clean/test generate registries/up
	./scripts/go-test-record ./...

# test/integration runs all integration tests after building and installing the
# latest Docker image in the local registry. It also disables kuberecord and
# copyist in order to always run against real K8s clusters. This command is
# useful when you want to run tests in an environment that is as close to
# production as possible. But since it's so much slower than "test", it's not
# part of the usual dev workflow.
.PHONY: test/integration
test/integration: clean/test generate push/local
	go test -tags=integration -count=1 -p=1 -v -timeout 120m ./...

# test/race runs all tests with the -race flag in order to detect data races in
# the code. This is very slow.
.PHONY: test/race
test/race: clean/test generate
	KUBERECORD=disable go test ./... -p=1 -v -race

# clean resets operator building/testing to a clean state by stopping the local
# registry, destroying any known local test clusters, removing the bin
# directory, and clearing the Go test cache.
.PHONY: clean
clean: clean/test registries/down
	@if [ -d bin ]; then \
		rm -R bin; \
		go clean -testcache; \
	fi

# ------------------------------------------------------------------------------
# All rules below this comment are helper rules and should rarely be used
# directly by callers.
# ------------------------------------------------------------------------------

# registries/up ensures that Docker registries are running locally, which
# increases performance and allows offline development.
#
#   1. Three "caching" Docker registries are run locally. These registries pull
#      images from quay.io, docker.io, and/or us.gcr.io, and then cache them
#      locally for future pulls of those same images.
#
#   2. Another Docker registry is run on localhost:5000 so that custom images
#      can be pushed/pulled to/from there without ever touching a remote
#      registry. For example, this is how the cockroach-operator image can be
#      tested without write access to our repos on us.gcr.io.
#
.PHONY: registries/up
registries/up:
	@if [ "`docker ps -f name=registry.localhost -q`" = "" ]; then \
		echo "$(CYAN)Starting local Docker registry (for fast offline image push/pull)...$(NC)"; \
		cd pkg/infrastructure/k3d; ./registries.sh up; \
	fi

# registries/down stops the local Docker registries.
.PHONY: registries/down
registries/down:
	@if [ "`docker ps -f name=registry.localhost -q`" != "" ]; then \
		echo "$(CYAN)Stopping local Docker registry (for fast offline image push/pull)...$(NC)"; \
		cd pkg/infrastructure/k3d; ./registries.sh down; \
	fi

# ------------------------------------------------------------------------------
# Build and test rules.
# ------------------------------------------------------------------------------

# push/local builds a Docker image and pushes it to the local registry.
.PHONY: push/local
push/local: registries/up generate
	@echo "$(CYAN)Pushing image to local registry...$(NC)"
	bazel run :push_local

# push/staging builds a Docker image and pushes it to the development registry.
.PHONY: push/dev
push/dev: generate
	@echo "$(CYAN)Pushing image to development registry...$(NC)"
	bazel run :push_development --stamp

.PHONY: push/prod
push/prod: generate
	@echo "$(CYAN)Pushing image to production registry (UBI)...$(NC)"
	bazel run --config=ubi :push_production_ubi --stamp
	bazel run --config=ubi //services/init-container:push_production_ubi --stamp
	bazel run --config=ubi //services/inotifywait:push_production_ubi --stamp

# fmt formats Go code.
.PHONY: fmt
fmt:
	@go fmt ./...

# clean/test deletes any well-known shared clusters used for testing.
.PHONY: clean/test
clean/test:
	@echo "$(CYAN)Delete well-known test clusters...$(NC)"
	k3d cluster delete single-zone-test
	k3d cluster delete multi-zone-test

# ------------------------------------------------------------------------------
# Code and manifest generation rules.
# ------------------------------------------------------------------------------

# generate ensures that all generated code and config is up-to-date.
generate:
	@"$(MAKE)" generate-bazel
	@"$(MAKE)" generate-go
	@"$(MAKE)" generate-controller
	@"$(MAKE)" modernize
	go fmt ./...

# Use the process ID of this process so that concurrent runs of make don't
# clobber eachothers pids.
START_BZL_PID_FILE=.bazel-start-make.$$PPID.pid

# If a bazel server starts up within the make process, make will hang waiting
# for it to finish.  In order to get around this, we can start the server in a
# submake process and then kill it.
# The server will remain running since it runs in a global process and our other
# bazel calls can make use of it without blocking the top level Make process
# from exiting.
.PHONY: start-bazel-server
start-bazel-server:
	@# clean up any preexisting pid file before starting.  Note that we can't
	@# just remove all other pidfiles here in case there are concurrent runs of
	@# this process occuring.
	@rm -f $(START_BZL_PID_FILE)
	@# Spawn a non-blocking process to start checking for the pid file before
	@# running make in a subprocess. We do it before because the submake process
	@# doesn't return when it starts the server.
	@( ./scripts/wait-for-pid-and-kill.sh $(START_BZL_PID_FILE) 25 ) & 2>&1 >/dev/null
	@# Pass in the pidfile name to the subprocess so that it doesn't regenerate
	@# the pidfile with its own pid. Force a successful response since we're
	@# killing it.
	@$(MAKE) start-bazel-server-subprocess START_BZL_PID_FILE=$(START_BZL_PID_FILE)  2>&1 >/dev/null || true

.PHONY: start-bazel-server-subprocess
start-bazel-server-subprocess:
	@# Run bazel info to start the bazel server, if it is already started this
	@# will quickly exit
	@bazel info 2>&1 >/dev/null
	@echo $$PPID > $(START_BZL_PID_FILE)

.PHONY: generate-bazel
generate-bazel: start-bazel-server
	./scripts/generate-bazel.sh

generate-go:
	bazel run //build/gen:proto
	bazel run //build/gen:gomock

generate-controller: bin/controller-gen
	@echo "$(CYAN)Generating CRD yaml files...$(NC)"
	bin/controller-gen crd:crdVersions=v1,allowDangerousTypes=true,generateEmbeddedObjectMeta=true paths="./..." output:crd:artifacts:config=manifests/internal/yaml
	@echo "$(CYAN)Generating deepcopy code...$(NC)"
	bin/controller-gen object:headerFile="boilerplate.go.txt" paths="./..."

modernize: bin/modernize
	./bin/modernize -fix -test ./...

# ------------------------------------------------------------------------------
# Tool build rules.
# ------------------------------------------------------------------------------

bin/controller-gen:
	@echo "$(CYAN)Installing controller-gen (K8s Operator SDK Go code generator)...$(NC)"
	$(GO_INSTALL) sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

bin/helm:
	@echo "$(CYAN)Installing helm...$(NC)"
	@mkdir -p bin
	@curl -L https://get.helm.sh/helm-v3.8.1-$(OS_NAME)-$(ARCH_NAME).tar.gz | tar -xzf - -C bin/ --strip-components 1
	@rm -f bin/README.md bin/LICENSE