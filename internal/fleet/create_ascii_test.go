package fleet

// P75：user-data 非 ASCII 字节拦截测试（DO 链路 latin-1 roundtrip 二次编码）。

import (
	"strings"
	"testing"
)

func TestCheckASCIIBlocksNonASCII(t *testing.T) {
	ascii := "#!/bin/bash\necho ok\n"
	if err := checkASCII(ascii, "team2"); err != nil {
		t.Fatalf("纯 ASCII 应放行：%v", err)
	}
	nonASCII := "#!/bin/bash\n# 中文注释\necho ok\n"
	err := checkASCII(nonASCII, "team2")
	if err == nil {
		t.Fatal("非 ASCII 应拦截")
	}
	if !strings.Contains(err.Error(), "非 ASCII") {
		t.Fatalf("报错应含 P75 提示：%v", err)
	}
}
