package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist 是可直接随 Go 二进制发布的 WebUI 产物。
//
//go:embed dist/*
var dist embed.FS

// Handler 提供静态资源和 history 路由回退，/api 路径由 httpapi 单独处理。
func Handler() http.Handler {
	staticFS, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/api/") {
			http.NotFound(writer, request)
			return
		}
		path := strings.TrimPrefix(request.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(staticFS, path); err != nil {
			// Vue history 模式下，未知前端路径需要回退首页。
			request.URL.Path = "/"
		}
		files.ServeHTTP(writer, request)
	})
}
