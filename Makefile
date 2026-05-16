# PulseGuard Makefile
#
# 常用：
#   make build      本平台编译
#   make test       全量单测
#   make cover      带覆盖率运行
#   make lint       vet + 编译（轻量 lint）
#   make docker     构建 docker 镜像 pulseguard:dev
#   make release    跨平台编译产出 dist/

.PHONY: build test fmt vet run smoke cover lint release docker clean

BINARY      := pulseguard
PKG         := ./cmd/pulseguard
DIST_DIR    := dist
LDFLAGS     := -s -w

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)

test:
	go test ./...

cover:
	go test ./... -cover -count=1

lint:
	go vet ./...
	go build ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

run: build
	./$(BINARY)

smoke:
	@go run ./scripts/smoke -bot-token "$$PULSEGUARD_SMOKE_BOT_TOKEN" -chat-id "$$PULSEGUARD_SMOKE_CHAT_ID"

# ─── 镜像 ─────────────────────────────────────────────────────────────
docker:
	docker build -t pulseguard:dev .

# ─── 跨平台发布 ─────────────────────────────────────────────────────────
# 目标矩阵：linux/{amd64,arm64}, windows/amd64, darwin/{amd64,arm64}
# 输出统一到 dist/pulseguard-<os>-<arch>[.exe]
release:
	@mkdir -p $(DIST_DIR)
	@echo ">> linux/amd64"
	@CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-amd64       $(PKG)
	@echo ">> linux/arm64"
	@CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-linux-arm64       $(PKG)
	@echo ">> windows/amd64"
	@CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-windows-amd64.exe $(PKG)
	@echo ">> darwin/amd64"
	@CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-darwin-amd64      $(PKG)
	@echo ">> darwin/arm64"
	@CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST_DIR)/$(BINARY)-darwin-arm64      $(PKG)
	@echo "release artifacts -> $(DIST_DIR)/"

clean:
	rm -rf $(DIST_DIR) $(BINARY) $(BINARY).exe
