package main

import "Abot/internal/app"

// 发布二进制只通过 app 进行装配，避免入口文件感知业务模块。
func main() {
	app.Main()
}
