package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxRuntimeOptionsOverrideBytes = 32 << 10

// EncodeRuntimeOptionsOverride 编码排队任务需要恢复的无秘密运行参数。系统指令
// 属于提示词正文，不复制进任务元数据；恢复时由当前 persona/配置补回。
func EncodeRuntimeOptionsOverride(value RuntimeOptions) (string, error) {
	value.Instruction = ""
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("编码运行参数快照失败: %w", err)
	}
	if len(encoded) > maxRuntimeOptionsOverrideBytes {
		return "", errors.New("运行参数快照超过大小上限")
	}
	return string(encoded), nil
}

// DecodeRuntimeOptionsOverride 只接受单一有界 JSON 文档，供 Runtime 重启后
// 从 Invocation 恢复 Follow-up 的实际运行配置。
func DecodeRuntimeOptionsOverride(encoded string) (RuntimeOptions, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return RuntimeOptions{}, errors.New("运行参数快照为空")
	}
	if len(encoded) > maxRuntimeOptionsOverrideBytes {
		return RuntimeOptions{}, errors.New("运行参数快照超过大小上限")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	var value RuntimeOptions
	if err := decoder.Decode(&value); err != nil {
		return RuntimeOptions{}, fmt.Errorf("解析运行参数快照失败: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return RuntimeOptions{}, errors.New("运行参数快照包含尾部 JSON")
		}
		return RuntimeOptions{}, fmt.Errorf("解析运行参数快照尾部失败: %w", err)
	}
	value.Instruction = ""
	return value, nil
}
