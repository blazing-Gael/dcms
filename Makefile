VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet fmt tidy run clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/dcms ./cmd/dcms

test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

tidy:
	go mod tidy

run: build
	./bin/dcms

clean:
	rm -rf bin
