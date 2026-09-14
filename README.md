# ayase-abot

ayase-abot 是一个使用 Go 和 Vue 构建的自托管 AI Agent 运行平台。项目提供统一的 Web 管理界面，将模型供应商、对话、工作区、工具调用、人工审批、长期记忆和机器人接入集中到一个可持久化的运行环境中。

后端以内嵌 SQLite 保存配置与运行状态，并通过 REST、SSE 和 WebSocket 提供服务；前端构建产物会嵌入 Go 二进制，因此正式部署时只需要运行一个可执行文件。

## 主要能力

- 支持 OpenAI 兼容接口和 Gemini 模型供应商。
- 管理对话、人格、长期记忆、模型配置和运行状态。
- 将本地或远程目录注册为 Agent 工作区。
- 提供文件读取、搜索、修改、命令执行和 Git 操作能力。
- 对敏感工作区操作进行人工批准或拒绝，并保留恢复所需的运行状态。
- 支持命令实时日志、PTY 会话、执行取消和产物归档。
- 支持 Telegram 与 OneBot 11 机器人接入。
- 提供运行追踪、能力探测、评测和发布门禁。

## 技术栈

- Go 1.27
- Vue 3、TypeScript、Vite、Pinia、Naive UI
- SQLite
- Google ADK for Go

## 环境要求

- Go 1.27 或更高版本
- Node.js 22.20 或更高版本
- npm
- GNU Make（可选，用于执行统一的检查命令）

## 构建

在仓库根目录安装前端依赖并生成 Web UI：

```bash
npm --prefix web ci
npm --prefix web run build
```

随后构建后端和最终可执行文件：

```bash
go build -o ayase-abot ./cmd/abot
```

启动服务：

```bash
./ayase-abot
```

默认监听 `127.0.0.1:8080`，运行数据写入 `./data`。浏览器访问 <http://127.0.0.1:8080> 后，可在管理界面中配置模型供应商、默认模型、工作区和机器人。

## 本地开发

首次开发前安装前端依赖：

```bash
npm --prefix web ci
```

开发时分别启动后端和 Vite 开发服务器。前端开发代理默认将 `/api` 请求转发至 `127.0.0.1:6185`。

终端一：

```bash
go run . -http 127.0.0.1:6185
```

终端二：

```bash
npm --prefix web run dev
```

浏览器访问 <http://127.0.0.1:5173>。修改 Go 代码后需要重启后端；Vue 和 TypeScript 代码由 Vite 热更新。

如果只开发后端或使用已经生成的前端资源，也可以直接运行：

```bash
go run .
```

## 配置

启动参数优先于环境变量：

| 启动参数 | 环境变量 | 默认值 | 用途 |
| --- | --- | --- | --- |
| `-http` | `ABOT_HTTP_ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `-data` | `ABOT_DATA_DIR` | `./data` | SQLite、产物和运行数据目录 |
| `-log-level` | `ABOT_LOG_LEVEL` | `info` | 启动日志等级 |

示例：

```bash
ABOT_DATA_DIR=/var/lib/ayase-abot \
  ./ayase-abot -http 0.0.0.0:8080 -log-level debug
```

模型密钥、机器人凭据和日常业务配置应通过 Web 管理界面设置，不需要写入源码。

## 测试与质量检查

运行 Go 测试：

```bash
make test
```

运行前端类型检查和构建：

```bash
make web-check
```

运行完整的本地 CI，包括普通测试、竞态检测、`go vet`、前端检查和能力门禁冒烟测试：

```bash
make ci
```

也可以分别执行：

```bash
make test-race
make vet
make capability-gate-smoke
```

使用自定义模型能力报告执行门禁：

```bash
make capability-gate CAPABILITY_GATE_INPUT=/path/to/report.json
```

## 项目结构

```text
.
├── cmd/                 # 可执行程序入口
├── internal/            # Agent、API、存储、工作区和集成实现
├── web/                 # Vue Web UI 源码
├── ci/                  # CI 使用的固定测试输入
├── data/                # 本地运行数据，不进入版本控制
├── Makefile             # 测试与质量检查入口
└── go.mod               # Go 模块依赖
```

提交代码前建议至少运行 `make test` 和 `make web-check`；影响并发、命令执行或持久化逻辑的变更应运行完整的 `make ci`。
