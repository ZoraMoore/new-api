package middleware

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func CORS() gin.HandlerFunc {
	config := cors.DefaultConfig()
	// 浏览器不允许带 credentials 的请求使用 Access-Control-Allow-Origin: *。
	// 这里保持“允许任意来源”的兼容性，但让 gin-contrib/cors 回显具体 Origin。
	config.AllowOriginFunc = func(origin string) bool {
		return origin != ""
	}
	config.AllowCredentials = true
	config.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	config.AllowHeaders = []string{
		"Origin",
		"Content-Length",
		"Content-Type",
		"Authorization",
		"Accept",
		"Cache-Control",
		"X-Requested-With",
		"X-API-Key",
		"Api-Key",
		"OpenAI-Organization",
		"OpenAI-Project",
		"Anthropic-Version",
		"Anthropic-Beta",
		"HTTP-Referer",
		"X-Title",
		"New-Api-User",
		"X-New-Api-User",
		"X-Stainless-Arch",
		"X-Stainless-Lang",
		"X-Stainless-Os",
		"X-Stainless-Package-Version",
		"X-Stainless-Runtime",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Retry-Count",
		"X-Stainless-Timeout",
	}
	return cors.New(config)
}

func PoweredBy() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-New-Api-Version", common.Version)
		c.Next()
	}
}
