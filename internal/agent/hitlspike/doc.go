// Package hitlspike 是 Abot Agent Runtime Phase 0 的技术验证代码。
//
// 依据 AGENT_DOCUMENT_EXECUTION_ORDER.md 第 6 节，Phase 0 的职责是把“ADK 的 HITL、
// 暂停与恢复事实”固化成可重复执行的测试，而不是靠推测补齐 ADK 语义。本包只包含
// 确定性、无网络、无真实副作用的实验：
//
//   - 模型是脚本化的（internal/agent/hitlspike 内的 scriptedModel），不访问供应商；
//   - 工具只写自己的 JSONL 账本文件，不写工作区、不执行命令；
//   - 会话使用与生产一致的 ADK SQLite session service，用来验证跨进程恢复。
//
// 它不接入生产 Kernel、HTTP API 或 WebUI，也不修改现有 Workspace 写工具。
// 部署物是测试、事件 fixture 与结论记录，见 AGENT_PHASE0_ADK_HITL_FINDINGS.md。
//
// 本包只有测试文件，除本文件外没有非测试代码，因此不会被任何生产二进制引用。
package hitlspike
