.PHONY: image

IMAGE?=nats-operator

# Where to push the docker image.
REGISTRY ?= innoq
#VERSION := $(shell git describe --tags --always --dirty)
VERSION := 0.0.1

image: nats-operator
	docker build -t $(REGISTRY)/$(IMAGE):$(VERSION) -f Dockerfile.scratch .

nats-operator: $(shell find . -name "*.go")
	glide install -v
	CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o nats-operator ./cmd/nats-operator/.

.PHONY: clean
clean:
	rm -rf vendor
	rm nats-operator
