IMAGE ?= amneziawg-mikrotik
TAG   ?= latest
DIST  ?= dist
BUILDER ?= amneziawg-mikrotik

# RouterOS imports a docker-archive tar (`/container add file=...`).
build-%: builder
	@mkdir -p $(DIST)
	docker buildx build --builder $(BUILDER) --platform $(PLATFORM_$*) \
		--provenance=false --sbom=false \
		-t $(IMAGE):$(TAG) \
		-o type=docker,dest=$(DIST)/$(IMAGE)-$*.tar .

PLATFORM_arm64 = linux/arm64
PLATFORM_armv7 = linux/arm/v7
PLATFORM_amd64 = linux/amd64

.PHONY: all builder test qemu clean
all: build-arm64 build-armv7 build-amd64

# The default "docker" driver cannot export tarballs or cross-build; use docker-container.
builder:
	@docker buildx inspect $(BUILDER) >/dev/null 2>&1 || docker buildx create --name $(BUILDER) --driver docker-container >/dev/null

test:
	cd backend && go vet ./... && go test ./...

# Register QEMU binfmt handlers so the C stage can build for ARM targets.
qemu:
	docker run --privileged --rm tonistiigi/binfmt --install arm64,arm

clean:
	rm -rf $(DIST)
