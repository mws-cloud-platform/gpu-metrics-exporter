IMAGE_NAME  := gpu-metrics-exporter-builder
CT_NAME := gpu-metrics-exporter-extractor
BIN_DIR     := bin
DIST_DIR    := dist
DOCKER_IMAGE_NAME ?= gpu-metrics-exporter:latest
# Deb version; empty -> scripts/build-deb.sh falls back to `git describe --tags --always`.
VERSION     ?=
# Pinned so `make lint` and CI run byte-identical checks. Must be built with a Go
# toolchain >= go.mod's `go` directive; the Debian image (not -alpine) is required
# because go-nvml is cgo and needs a C compiler.
GOLANGCI_IMAGE ?= golangci/golangci-lint:v2.13.1

.PHONY: gpu-metrics-exporter gpu-metrics-receiver docker-build docker-image deb docker-test fmt vet lint test cover clean

docker-image:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -f docker/k8s.Dockerfile -t $(DOCKER_IMAGE_NAME) .


gpu-metrics-exporter: cmd/gpu-metrics-exporter/main.go
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o ./gpu-metrics-exporter cmd/gpu-metrics-exporter/main.go


gpu-metrics-receiver: cmd/gpu-metrics-receiver/main.go
	GOOS=linux GOARCH=amd64 go build -tags osusergo,netgo -o ./gpu-metrics-receiver  cmd/gpu-metrics-receiver/main.go


docker-build:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -f docker/deb.Dockerfile -t $(IMAGE_NAME) .
	mkdir -p $(BIN_DIR)
	@docker rm -f $(CT_NAME) >/dev/null 2>&1 || true
	docker create --name $(CT_NAME) $(IMAGE_NAME)
	docker cp $(CT_NAME):/gpu-metrics-exporter $(BIN_DIR)/gpu-metrics-exporter
	docker cp $(CT_NAME):/gpu-metrics-receiver $(BIN_DIR)/gpu-metrics-receiver
	docker rm $(CT_NAME) >/dev/null 2>&1


# Build both .deb packages from bin/ via dpkg-deb + the debian/*/DEBIAN templates.
deb: docker-build
	@mkdir -p $(DIST_DIR)
	scripts/build-deb.sh gpu-metrics-exporter "$(VERSION)"
	scripts/build-deb.sh gpu-metrics-receiver "$(VERSION)"


docker-test:
	docker build --platform linux/amd64 -f docker/deb.Dockerfile --target builder -t $(IMAGE_NAME)-test .
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
	docker build --platform linux/amd64 -f docker/deb.Dockerfile --target builder -t $(IMAGE_NAME)-test .
	docker run --rm --platform linux/amd64 $(IMAGE_NAME)-test \
		sh -c 'go test -count=1 -coverprofile=/tmp/cover.out ./... >/dev/null && go tool cover -func=/tmp/cover.out | tail -1'


clean:
	rm -f ./gpu-metrics-exporter  ./gpu-metrics-receiver
	rm -rf $(BIN_DIR) $(DIST_DIR)
