IMAGE_NAME  := gpu-metrics-exporter-builder
CT_NAME := gpu-metrics-exporter-extractor
BIN_DIR     := bin
DIST_DIR    := dist
DOCKER_IMAGE_NAME ?= gpu-metrics-exporter:latest
DOCKERFILE  := docker/Dockerfile
# Deb / image version; empty -> falls back to `git describe --tags --always`.
VERSION     ?= $(shell git describe --tags --always 2>/dev/null || echo "")
# Pinned so `make lint` and CI run byte-identical checks. Must be built with a Go
# toolchain >= go.mod's `go` directive; the Debian image (not -alpine) is required
# because go-nvml is cgo and needs a C compiler.
GOLANGCI_IMAGE ?= golangci/golangci-lint:v2.13.1
# pci-attest signing (scripts/sign-elf.py). ATTEST_VERSION is the monotonic
# release number signed into the manifest: the host refuses anything below its
# min-version. It comes from the ATTEST_VERSION file, so a release signs with
# the number in its own tagged commit -- re-releasing old code yields the old
# number, which the floor still refuses. Raise it in the commit that fixes what
# the hosts' min-version is raised to refuse out; never lower it. A command-line
# ATTEST_VERSION=N overrides it for local experiments only.
# SIGN_KEY is the operator's RSA private key (PEM); without it `make deb`
# packages an unsigned exporter, whose sends the device ignores. VERIFY_KEY is
# the matching public key as the hosts' QEMU gets it (pubkey=): operator-pub.der,
# PEM, or base64 of the DER.
ATTEST_VERSION := $(shell cat ATTEST_VERSION 2>/dev/null)
SIGN_KEY       ?=
VERIFY_KEY     ?=
CHECK_ATTEST_VERSION = case "$(ATTEST_VERSION)" in ''|*[!0-9]*) echo "error: ATTEST_VERSION must be a non-negative integer (see the ATTEST_VERSION file), not '$(ATTEST_VERSION)'" >&2; exit 1;; esac

.PHONY: gpu-metrics-exporter gpu-metrics-receiver docker-build docker-image sign verify-signature deb package docker-test fmt vet lint test cover clean

docker-image:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) --target exporter -f $(DOCKERFILE) -t $(DOCKER_IMAGE_NAME) .


gpu-metrics-exporter: cmd/gpu-metrics-exporter/main.go
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-linkmode external -extldflags "-no-pie -Wl,-z,separate-code"' -o ./gpu-metrics-exporter cmd/gpu-metrics-exporter/main.go


gpu-metrics-receiver: cmd/gpu-metrics-receiver/main.go
	GOOS=linux GOARCH=amd64 go build -tags osusergo,netgo -o ./gpu-metrics-receiver  cmd/gpu-metrics-receiver/main.go


docker-build:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) --target binaries -f $(DOCKERFILE) -t $(IMAGE_NAME) .
	mkdir -p $(BIN_DIR)
	@docker rm -f $(CT_NAME) >/dev/null 2>&1 || true
	docker create --name $(CT_NAME) $(IMAGE_NAME)
	docker cp $(CT_NAME):/gpu-metrics-exporter $(BIN_DIR)/gpu-metrics-exporter
	docker cp $(CT_NAME):/gpu-metrics-receiver $(BIN_DIR)/gpu-metrics-receiver
	docker rm $(CT_NAME) >/dev/null 2>&1


# Sign bin/gpu-metrics-exporter in place: hash its code pages and patch the
# signed manifest into the reserved .note.attest. Runs after docker-build;
# nothing may modify the binary afterwards, or its pages stop matching.
sign:
	@if [ -z "$(SIGN_KEY)" ]; then echo "error: set SIGN_KEY=<operator RSA private key, PEM>" >&2; exit 1; fi
	@$(CHECK_ATTEST_VERSION)
	python3 scripts/sign-elf.py --key "$(SIGN_KEY)" -n gpu-metrics-exporter --version $(ATTEST_VERSION) $(BIN_DIR)/gpu-metrics-exporter


# Check the signed exporter in BIN_DIR the way the pci-attest device will:
# the note through the program headers, the signature against VERIFY_KEY, the
# manifest against the binary's own code pages, and ATTEST_VERSION.
verify-signature:
	@if [ -z "$(VERIFY_KEY)" ]; then echo "error: set VERIFY_KEY=<operator public key: operator-pub.der, PEM or base64>" >&2; exit 1; fi
	@$(CHECK_ATTEST_VERSION)
	python3 scripts/sign-elf.py --verify "$(VERIFY_KEY)" -n gpu-metrics-exporter --version $(ATTEST_VERSION) $(BIN_DIR)/gpu-metrics-exporter


# Build both .deb packages from bin/ via dpkg-deb + the debian/*/DEBIAN templates.
# With SIGN_KEY set, the exporter is signed before it is packaged.
deb: docker-build
ifneq ($(strip $(SIGN_KEY)),)
	$(MAKE) sign
else
	@echo "warning: SIGN_KEY is not set, packaging an unsigned gpu-metrics-exporter" >&2
endif
	$(MAKE) package


# Package bin/ as it stands, without rebuilding it: the release job signs
# between docker-build and this, so the key is only around for that one step.
package:
	@mkdir -p $(DIST_DIR)
	scripts/build-deb.sh gpu-metrics-exporter "$(VERSION)"
	scripts/build-deb.sh gpu-metrics-receiver "$(VERSION)"


docker-test:
	docker build --platform linux/amd64 -f $(DOCKERFILE) --target builder -t $(IMAGE_NAME)-test .
	docker run --rm --platform linux/amd64 $(IMAGE_NAME)-test go test -v -count=1 ./...


fmt:
	gofmt -w .


vet:
	go vet ./...


# Same image and flags CI uses, so a green local run means a green CI run.
lint:
	docker run --rm --platform linux/amd64 \
		-v "$(CURDIR)":/src -w /src -e GOFLAGS=-buildvcs=false \
		$(GOLANGCI_IMAGE) golangci-lint run --timeout 5m


test:
	go test -v ./...


# Coverage across all packages. Runs in the build container so linux-only tests
# (see pkg/vsock/common/deadline_vsock_test.go) are included in the figure.
cover:
	docker build --platform linux/amd64 -f $(DOCKERFILE) --target builder -t $(IMAGE_NAME)-test .
	docker run --rm --platform linux/amd64 $(IMAGE_NAME)-test \
		sh -c 'go test -count=1 -coverprofile=/tmp/cover.out ./... >/dev/null && go tool cover -func=/tmp/cover.out | tail -1'


clean:
	rm -f ./gpu-metrics-exporter  ./gpu-metrics-receiver
	rm -rf $(BIN_DIR) $(DIST_DIR)
