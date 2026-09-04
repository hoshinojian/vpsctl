BIN_DIR := bin
BINARY  := $(BIN_DIR)/vpsctl

.PHONY: build lint test hooks clean

build:
	go build -o $(BINARY) ./cmd/vpsctl

lint:
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then \
		echo "gofmt 未格式化文件:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

test:
	go test -race ./...

# 安装本地 pre-push 钩子（lint+test 全绿、禁止直推 main）
hooks:
	git config core.hooksPath .githooks

clean:
	rm -rf $(BIN_DIR)
