BIN_DIR := bin
BINARY  := $(BIN_DIR)/vpsctl

.PHONY: build lint test check-ui hooks clean

build:
	go build -o $(BINARY) ./cmd/vpsctl

lint:
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then \
		echo "gofmt 未格式化文件:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

test:
	go test -race ./...

# 前端冒烟：DOM 桩 + 固定载荷完整执行页面脚本，拦截求值/渲染期整页死症（需 node）
check-ui:
	node tools/check-ui.mjs

# 安装本地 pre-push 钩子（lint+test 全绿、禁止直推 main）
hooks:
	git config core.hooksPath .githooks

clean:
	rm -rf $(BIN_DIR)
