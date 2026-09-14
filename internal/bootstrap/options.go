package bootstrap

import (
	"flag"
	"os"
)

// Options 只包含进程启动所必需的引导参数；日常业务配置全部保存在 SQLite。
type Options struct {
	HTTPAddr string
	DataDir  string
	LogLevel string
}

// Parse 从命令行和环境变量读取启动参数，命令行优先于环境变量。
func Parse(args []string) Options {
	httpDefault := getenv("ABOT_HTTP_ADDR", "127.0.0.1:8080")
	dataDefault := getenv("ABOT_DATA_DIR", "./data")
	logDefault := getenv("ABOT_LOG_LEVEL", "info")

	flags := flag.NewFlagSet("abot", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	addr := flags.String("http", httpDefault, "HTTP 监听地址")
	dataDir := flags.String("data", dataDefault, "运行时数据目录")
	logLevel := flags.String("log-level", logDefault, "启动日志级别")
	_ = flags.Parse(args)

	return Options{HTTPAddr: *addr, DataDir: *dataDir, LogLevel: *logLevel}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
