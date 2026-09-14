// Package contextspike 是 Abot Agent Runtime Phase 0 的第三块技术验证：
// ADK BeforeModel callback 与 compaction 的执行顺序。
//
// 依据 AGENT_IMPLEMENTATION_ROADMAP.md 第 7 节（“BeforeModel callback 与 compaction 顺序”）
// 与 AGENT_CONTEXT_ENGINE.md 第 29.4、30 节提出的问题：
//
//   - BeforeModel callback 收到的 request 是否已经完成 compaction materialization；
//   - callback 修改 request 后 Session 原始事件是否保持不变；
//   - 工具循环的每次模型调用是否都会触发 callback；
//   - compaction 事件能否稳定映射为 AgentEvent；
//   - compaction 是否影响等待审批的恢复。
//
// 本包只做验证，不接入生产 Kernel；除本文件外没有非测试代码。
package contextspike
