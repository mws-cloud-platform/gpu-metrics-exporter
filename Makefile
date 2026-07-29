IMAGE_NAME  := gpu-metrics-exporter-builder
CT_NAME := gpu-metrics-exporter-extractor
BIN_DIR     := bin
DIST_DIR    := dist
# Deb version; empty -> scripts/build-deb.sh falls back to `git describe --tags --always`.
VERSION     ?=

.PHONY: gpu-metrics-exporter gpu-metrics-receiver docker-build deb docker-test fmt vet test clean

gpu-metrics-exporter: cmd/gpu-metrics-exporter/main.go
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o ./gpu-metrics-exporter cmd/gpu-metrics-exporter/main.go


gpu-metrics-receiver: cmd/gpu-metrics-receiver/main.go
	GOOS=linux GOARCH=amd64 go build -tags osusergo,netgo -o ./gpu-metrics-receiver  cmd/gpu-metrics-receiver/main.go


docker-build:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -f docker/local-build.Dockerfile -t $(IMAGE_NAME) .
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
	docker build --platform linux/amd64 -f docker/local-build.Dockerfile --target builder -t $(IMAGE_NAME)-test .
	docker run --rm --platform linux/amd64 $(IMAGE_NAME)-test go test -v -count=1 ./...


fmt:
	gofmt -w .


vet:
	go vet ./...


test:
	go test -v ./...


clean:
	rm -f ./gpu-metrics-exporter  ./gpu-metrics-receiver
	rm -rf $(BIN_DIR) $(DIST_DIR)
