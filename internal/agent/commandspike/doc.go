// Package commandspike 是 Abot Agent Runtime Phase 0 的第二块技术验证：
// 本地命令执行的进程组、stdout/stderr 采集和取消语义。
//
// 依据 AGENT_IMPLEMENTATION_ROADMAP.md 第 7 节（“本地 process group、stdout/stderr、cancel”）
// 与 COMMAND_RUNTIME.md 第 7、9、12 节的行為约定。本包只做验证，不接入生产 Kernel：
//
//   - 不接 HTTP/SSE、不写数据库；
//   - 只在本包测试内部实现一个最小 LocalProcess，用来暴露真实的 OS 语义；
//   - 结论固化在测试与 AGENT_PHASE0_ADK_HITL_FINDINGS.md 中。
//
// 仅适用于 Unix（依赖 setpgid 与进程组信号）；Windows 不在本阶段范围内。
//
// 除本文件外没有非测试代码，因此不会被任何生产二进制引用。
package commandspike
