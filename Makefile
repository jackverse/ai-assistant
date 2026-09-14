BINARY  := winclean.exe
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X winclean/internal/version.Version=$(VERSION) \
	-X winclean/internal/version.Commit=$(COMMIT) \
	-X winclean/internal/version.BuildTime=$(BUILT)

GOFLAGS := -trimpath

.PHONY: all build test vet fmt clean install size scan-c scan-d

all: build

## build: 编译到 bin/winclean.exe
build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/winclean

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
	gofmt -w ./cmd ./internal

## clean: 清理产物
clean:
	rm -rf bin winclean-scan.json

## install: 安装到 GOPATH/bin
install:
	go install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/winclean

## size: 显示产物大小
size: build
	@ls -lh bin/$(BINARY) | awk '{print "产物大小: " $$5}'

## scan-c: 快速扫描 C 盘
scan-c: build
	./bin/$(BINARY) scan --disk C

## scan-d: 深度扫描 C 盘
scan-d: build
	./bin/$(BINARY) scan --disk C --deep

## help: 显示本帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
