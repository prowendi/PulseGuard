.PHONY: build test fmt vet run smoke
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
smoke:
	@go run ./scripts/smoke -bot-token "$$PULSEGUARD_SMOKE_BOT_TOKEN" -chat-id "$$PULSEGUARD_SMOKE_CHAT_ID"
