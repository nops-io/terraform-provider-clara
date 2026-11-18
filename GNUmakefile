# Use a Go toolchain that meets go.mod. Tests drive the installed terraform binary.
.PHONY: build test vet fmt

build:
	go build ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

test:
	go test ./... -count=1
