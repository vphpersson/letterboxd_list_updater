CMD_BINARY     := letterboxd_list_updater
SERVICE_BINARY := letterboxd_list_updater_service
IMAGE          := letterboxd_list_updater
REGISTRY       := registry.home.arpa
GO_ENV         := GOEXPERIMENT=jsonv2

.PHONY: all update build build-cmd build-service test fmt vet image publish clean

all: build

update:
	@echo "[letterboxd_list_updater] Updating..."
	gm


build:
	$(GO_ENV) CGO_ENABLED=0 go build -ldflags="-s -w" -o $(SERVICE_BINARY) .

test:
	$(GO_ENV) go test ./...

fmt:
	gofmt -w .

vet:
	$(GO_ENV) go vet ./...

image:
	podman build -t $(IMAGE) .

publish: image
	podman tag $(IMAGE) $(REGISTRY)/$(IMAGE)
	podman push $(REGISTRY)/$(IMAGE)

clean:
	rm -f $(CMD_BINARY) $(SERVICE_BINARY)
