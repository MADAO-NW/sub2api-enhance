package web

import "embed"

// Assets 是当前增强服务自己的前端构建，不读取原版前端文件。
//
//go:embed dist
var Assets embed.FS
