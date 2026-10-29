package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kiro2api/auth"
	"kiro2api/converter"
	"kiro2api/logger"
	"kiro2api/types"
	"kiro2api/utils"

	"github.com/gin-gonic/gin"
)

// 移除全局httpClient，使用utils包中的共享客户端

// StartServer 启动HTTP代理服务器
func StartServer(port string, authToken string, authService *auth.AuthService) {
	adminServer, err := adminServerFromEnv(authService)
	if err != nil {
		logger.Error("管理接口配置无效", logger.Err(err))
		os.Exit(1)
	}
	if adminServer != nil {
		listener, err := net.Listen("tcp", adminServer.Addr)
		if err != nil {
			logger.Error("管理端口启动失败", logger.Err(err))
			os.Exit(1)
		}
		go func() {
			if err := adminServer.Serve(listener); err != nil && err != http.ErrServerClosed {
				logger.Error("管理接口运行失败", logger.Err(err))
			}
		}()
	}
	r := NewRouter(authToken, authService)
	logErrorDumpMode(authService.ManagedPool())
	server := &http.Server{Addr: net.JoinHostPort(os.Getenv("BIND_HOST"), port), Handler: r, ReadHeaderTimeout: 15 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if adminServer != nil {
			_ = adminServer.Shutdown(shutdown)
		}
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		if pool := authService.ManagedPool(); pool != nil {
			if err := pool.Close(); err != nil {
				logger.Error("账号统计保存失败", logger.Err(err))
			}
		}
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("启动服务器失败", logger.Err(err))
		stop()
	}
	<-done
}

func NewRouter(authToken string, authService *auth.AuthService) *gin.Engine {
	localDiscountCache = configuredDiscountCache()
	errorDumps.Store(errorDumpStoreFor(authService.ManagedPool()))
	// 设置 gin 模式
	ginMode := os.Getenv("GIN_MODE")
	if ginMode == "" {
		ginMode = gin.ReleaseMode
	}
	gin.SetMode(ginMode)

	r := gin.New()

	// 添加中间件
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	// 注入请求ID，便于日志追踪
	r.Use(RequestIDMiddleware())
	r.Use(corsMiddleware())
	// 只对 /v1 开头的端点进行认证
	if pool := authService.ManagedPool(); pool != nil {
		pool.SetBuiltinKey(authToken)
		r.Use(KeyAuthMiddleware(pool, []string{"/v1"}))
	} else {
		r.Use(PathBasedAuthMiddleware(authToken, []string{"/v1"}))
	}

	// 静态资源服务 - 前后端完全分离
	r.Static("/static", "./static")
	r.GET("/", func(c *gin.Context) {
		c.File("./static/index.html")
	})

	// API端点 - 纯数据服务
	if authService.ManagedPool() == nil {
		r.GET("/api/tokens", handleTokenPoolAPI)
	}

	// GET /v1/models 端点
	r.GET("/v1/models", handleModels)

	r.POST("/v1/messages", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<20)
		body, err := c.GetRawData()
		if err != nil {
			respondError(c, http.StatusBadRequest, "%s", "读取请求失败或请求体超过 32 MiB")
			return
		}
		rememberClientBody(c, body)

		// 先解析为通用map以便处理工具格式
		var rawReq map[string]any
		if err := utils.SafeUnmarshal(body, &rawReq); err != nil {
			logger.Error("解析请求体失败", logger.Err(err))
			respondError(c, http.StatusBadRequest, "解析请求体失败: %v", err)
			return
		}

		// 标准化工具格式处理
		if tools, exists := rawReq["tools"]; exists && tools != nil {
			if toolsArray, ok := tools.([]any); ok {
				normalizedTools := make([]map[string]any, 0, len(toolsArray))
				for _, tool := range toolsArray {
					if toolMap, ok := tool.(map[string]any); ok {
						// 检查是否是简化的工具格式（直接包含name, description, input_schema）
						if name, hasName := toolMap["name"]; hasName {
							if description, hasDesc := toolMap["description"]; hasDesc {
								if inputSchema, hasSchema := toolMap["input_schema"]; hasSchema {
									// 转换为标准Anthropic工具格式
									normalizedTool := map[string]any{
										"name":         name,
										"description":  description,
										"input_schema": inputSchema,
									}
									if control, exists := toolMap["cache_control"]; exists {
										normalizedTool["cache_control"] = control
									}
									normalizedTools = append(normalizedTools, normalizedTool)
									continue
								}
							}
						}
						// 如果不是简化格式，保持原样
						normalizedTools = append(normalizedTools, toolMap)
					}
				}
				rawReq["tools"] = normalizedTools
			}
		}

		// 重新序列化并解析为AnthropicRequest
		normalizedBody, err := utils.SafeMarshal(rawReq)
		if err != nil {
			logger.Error("重新序列化请求失败", logger.Err(err))
			respondError(c, http.StatusBadRequest, "处理请求格式失败: %v", err)
			return
		}

		var anthropicReq types.AnthropicRequest
		if err := utils.SafeUnmarshal(normalizedBody, &anthropicReq); err != nil {
			logger.Error("解析标准化请求体失败", logger.Err(err))
			respondError(c, http.StatusBadRequest, "解析请求体失败: %v", err)
			return
		}
		if !validateRequestedModel(c, anthropicReq.Model) {
			return
		}
		if !validateThinkingRequest(c, &anthropicReq) {
			return
		}

		// 验证请求的有效性
		if len(anthropicReq.Messages) == 0 {
			logger.Error("请求中没有消息")
			respondError(c, http.StatusBadRequest, "%s", "messages 数组不能为空")
			return
		}

		// 验证最后一条消息有有效内容
		lastMsg := anthropicReq.Messages[len(anthropicReq.Messages)-1]
		content, err := utils.GetMessageContent(lastMsg.Content)
		if err != nil {
			logger.Error("获取消息内容失败",
				logger.Err(err),
				logger.String("raw_content", fmt.Sprintf("%v", lastMsg.Content)))
			respondError(c, http.StatusBadRequest, "获取消息内容失败: %v", err)
			return
		}

		trimmedContent := strings.TrimSpace(content)
		if trimmedContent == "" || trimmedContent == "answer for user question" {
			logger.Error("消息内容为空或无效",
				logger.String("content", content),
				logger.String("trimmed_content", trimmedContent))
			respondError(c, http.StatusBadRequest, "%s", "消息内容不能为空")
			return
		}

		c.Set("request_stream", anthropicReq.Stream)
		// Claude Code 的 WebSearch：只带一个原生 web_search 工具，走 Kiro MCP 真实搜索
		if isPureWebSearchRequest(anthropicReq) {
			tokenWithUsage, release, ok := acquireRequestAccount(c, authService, anthropicReq.Model)
			if !ok {
				return
			}
			defer release()
			handleWebSearchRequest(c, anthropicReq, tokenWithUsage.TokenInfo)
			return
		}
		prepareCacheDiscount(c, anthropicReq)
		tokenWithUsage, release, ok := acquireRequestAccount(c, authService, anthropicReq.Model)
		if !ok {
			return
		}
		defer release()
		if anthropicReq.Stream {
			handleStreamRequest(c, anthropicReq, tokenWithUsage)
			return
		}

		handleNonStreamRequest(c, anthropicReq, tokenWithUsage.TokenInfo)
	})

	// Token计数端点
	r.POST("/v1/messages/count_tokens", handleCountTokens)

	// 新增：OpenAI兼容的 /v1/chat/completions 端点
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<20)
		body, err := c.GetRawData()
		if err != nil {
			respondError(c, http.StatusBadRequest, "%s", "读取请求失败或请求体超过 32 MiB")
			return
		}
		rememberClientBody(c, body)

		var openaiReq types.OpenAIRequest
		if err := utils.SafeUnmarshal(body, &openaiReq); err != nil {
			logger.Error("解析OpenAI请求体失败", logger.Err(err))
			respondError(c, http.StatusBadRequest, "解析请求体失败: %v", err)
			return
		}
		if !validateRequestedModel(c, openaiReq.Model) {
			return
		}

		logger.Debug("OpenAI请求解析成功",
			logger.String("model", openaiReq.Model),
			logger.Bool("stream", openaiReq.Stream != nil && *openaiReq.Stream),
			logger.Int("max_tokens", func() int {
				if openaiReq.MaxTokens != nil {
					return *openaiReq.MaxTokens
				}
				return 16384
			}()))

		// 转换为Anthropic格式
		anthropicReq := converter.ConvertOpenAIToAnthropic(openaiReq)
		if !validateThinkingRequest(c, &anthropicReq) {
			return
		}
		if len(anthropicReq.Messages) == 0 {
			respondError(c, http.StatusBadRequest, "%s", "messages 数组不能为空")
			return
		}
		c.Set("request_stream", anthropicReq.Stream)
		prepareCacheDiscount(c, anthropicReq)
		token, release, ok := acquireRequestAccount(c, authService, anthropicReq.Model)
		if !ok {
			return
		}
		defer release()
		if anthropicReq.Stream {
			handleOpenAIStreamRequest(c, anthropicReq, token.TokenInfo)
			return
		}
		handleOpenAINonStreamRequest(c, anthropicReq, token.TokenInfo)
	})

	r.NoRoute(func(c *gin.Context) {
		logger.Warn("访问未知端点",
			logger.String("path", c.Request.URL.Path),
			logger.String("method", c.Request.Method))
		respondError(c, http.StatusNotFound, "%s", "404 未找到")
	})

	return r
}

// corsMiddleware CORS中间件
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusOK)
			return
		}

		c.Next()
	}
}
