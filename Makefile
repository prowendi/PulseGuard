.PHONY: build test fmt vet run
build:
	go build -o pulseguard ./cmd/pulseguard
test:
	go test ./...
fmt:
	go fmt ./...
vet:
	go vet ./...
run: build
	./pulseguard
