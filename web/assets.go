// Package web 打包面板的前端文件，编译进二进制。
package web

import "embed"

//go:embed index.html style.css app.js share.js parse-proxy.js qrcode.js
var Files embed.FS
