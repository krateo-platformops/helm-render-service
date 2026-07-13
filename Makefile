BIN    := helm-render-service
IMAGE  ?= ghcr.io/braghettos/helm-render-service
TAG    ?= dev

.PHONY: build test vet fmt tidy docker run clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/$(BIN) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

docker:
	docker build -t $(IMAGE):$(TAG) .

run:
	go run .

clean:
	rm -rf bin
