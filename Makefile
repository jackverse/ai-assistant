BINARY  := winclean.exe
GUI     := 助手.exe
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X winclean/internal/version.Version=$(VERSION) \
	-X winclean/internal/version.Commit=$(COMMIT) \
	-X winclean/internal/version.BuildTime=$(BUILT)

GOFLAGS := -trimpath
# Wails 界面构建必须带 desktop,production 标签，
# 否则启动时弹「Wails applications will not build without the correct build tags」。
WAILSTAGS := -tags "desktop,production"

.PHONY: all build gui test vet fmt clean install size help

all: build gui

## build: 编译命令行版 bin/winclean.exe
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/winclean

## gui: 编译桌面版 bin/助手.exe（双击开界面，带参数走命令行）
gui:
	go build $(GOFLAGS) $(WAILSTAGS) -ldflags "$(LDFLAGS) -H windowsgui" -o "bin/$(GUI)" .

## test: 运行全部测试
test:
	go test ./...

## test-verbose: 运行全部测试（详细输出）
test-verbose:
	go test -v ./...

## vet: 静态检查
vet:
	go vet ./...

## fmt: 格式化
fmt:
	gofmt -w ./cmd ./internal *.go

## clean: 清理产物
clean:
	rm -rf bin winclean-scan.json winclean-report.html

## install: 安装命令行版到 GOPATH/bin
install:
	go install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/winclean

## help: 显示本帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
