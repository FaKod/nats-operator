.PHONY: image

IMAGE?=nats-operator

image: nats-operator
	docker build -t $(IMAGE) -f Dockerfile.scratch .

nats-operator: $(shell find . -name "*.go")
	glide install -v --strip-vcs
	CGO_ENABLED=0 GOARCH=amd64 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o nats-operator ./cmd/nats-operator/.

.PHONY: clean
clean:
	rm -rf vendor
	rm nats-operator
