package main

import (
	"os"
	"strconv"
	"time"

	"kiro2api/auth"
	"kiro2api/config"
	"kiro2api/converter"
	"kiro2api/logger"
	"kiro2api/server"

	"github.com/joho/godotenv"
)

func main() {
	// 自动加载.env文件
	if err := godotenv.Load(); err != nil {
		logger.Info("未找到.env文件，使用环境变量")
	}

	// 重新初始化logger以使用.env文件中的配置
	logger.Reinitialize()
	if err := config.LoadModelCatalog(os.Getenv("KIRO_MODELS_FILE")); err != nil {
		logger.Error("模型目录加载失败", logger.Err(err))
		os.Exit(1)
	}

	// 显示当前日志级别设置（仅在DEBUG级别时显示详细信息）
	// 注意：移除重复的系统字段，这些信息已包含在日志结构中
	logger.Debug("日志系统初始化完成",
		logger.String("config_level", os.Getenv("LOG_LEVEL")),
		logger.String("config_file", os.Getenv("LOG_FILE")))

	// Identity statement injected ahead of the client's system prompt.
	// Unset or empty keeps the built-in statement; "off" disables injection.
	converter.SetIdentityPrompt(os.Getenv("KIRO_IDENTITY_PROMPT"))

	// Client system lines hitting sensitive words are dropped before forwarding.
	// Unset or empty uses the built-in words; "off" disables; otherwise a file
	// whose words are appended to the built-in ones.
	if err := converter.SetSystemSensitiveWords(os.Getenv("KIRO_SYSTEM_SENSITIVE_WORDS")); err != nil {
		logger.Error("system 敏感词配置加载失败", logger.Err(err))
		os.Exit(1)
	}

	// How long a continued conversation waits for its busy account (seconds).
	if raw, present := os.LookupEnv("KIRO_STICKY_WAIT_SECONDS"); present {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 || seconds > 60 {
			logger.Error("KIRO_STICKY_WAIT_SECONDS 应为 0 到 60 的整数")
			os.Exit(1)
		}
		auth.SetStickyWait(time.Duration(seconds) * time.Second)
	}

	// Error scenes of failed requests under the admin data directory.
	// Unset or empty records; "off" disables; anything else is a mistake.
	if err := server.SetErrorDump(os.Getenv("KIRO_ERROR_DUMP")); err != nil {
		logger.Error("错误现场记录配置无效", logger.Err(err))
		os.Exit(1)
	}

	// 🚀 创建AuthService实例（使用依赖注入）
	logger.Info("正在创建AuthService...")
	authService, err := auth.NewAuthService()
	if err != nil {
		logger.Error("AuthService创建失败", logger.Err(err))
		logger.Error("请检查token配置后重新启动服务器")
		os.Exit(1)
	}

	// A model list saved from the admin page replaces KIRO_MODELS_FILE.
	if pool := authService.ManagedPool(); pool != nil {
		if path := pool.ModelCatalogPath(); fileExists(path) {
			if err := config.LoadModelCatalogFrom(path, "admin"); err != nil {
				logger.Error("管理页模型列表加载失败", logger.Err(err))
				os.Exit(1)
			}
		}
	}

	port := "8080" // 默认端口
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	// 从环境变量获取端口，覆盖命令行参数
	if envPort := os.Getenv("PORT"); envPort != "" {
		port = envPort
	}

	// 从环境变量获取客户端认证token（必需，无默认值）
	clientToken := os.Getenv("KIRO_CLIENT_TOKEN")
	if clientToken == "" {
		logger.Error("致命错误: 未设置 KIRO_CLIENT_TOKEN 环境变量")
		logger.Error("请在 .env 文件中设置强密码，例如: KIRO_CLIENT_TOKEN=your-secure-random-password")
		logger.Error("安全提示: 请使用至少32字符的随机字符串")
		os.Exit(1)
	}

	server.StartServer(port, clientToken, authService)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
