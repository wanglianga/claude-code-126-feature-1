package app

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:web/swui
var swuiFS embed.FS

//go:embed web/openapi.yaml
var openapiSpec []byte

func swaggerUI() http.Handler {
	sub, _ := fs.Sub(swuiFS, "web/swui")
	// swui/index.html 已定制为加载本服务 /openapi.yaml
	return http.StripPrefix("/swagger/", http.FileServer(http.FS(sub)))
}

func (a *App) openAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	_, _ = w.Write(openapiSpec)
}
