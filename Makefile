CMD_BINARY     := letterboxd_list_updater
SERVICE_BINARY := letterboxd_list_updater_service
IMAGE          := letterboxd_list_updater
REGISTRY       := registry.home.arpa

.PHONY: all update build build-cmd build-service test fmt vet image publish clean

all: build

update:
	@echo "[letterboxd_list_updater] Updating..."
	gm


build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o $(SERVICE_BINARY) .

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

image:
	podman build -t $(IMAGE) .

publish: image
	podman tag $(IMAGE) $(REGISTRY)/$(IMAGE)
	podman push $(REGISTRY)/$(IMAGE)

clean:
	rm -f $(CMD_BINARY) $(SERVICE_BINARY)
