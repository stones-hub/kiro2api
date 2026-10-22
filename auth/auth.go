package auth

import (
	"fmt"
	"kiro2api/logger"
	"kiro2api/types"
	"os"
	"path/filepath"
)

// AuthService 认证服务（推荐使用依赖注入方式）
type AuthService struct {
	managed      *AccountPool
	tokenManager *TokenManager
	configs      []AuthConfig
}

// NewAuthService 创建新的认证服务（推荐使用此方法而不是全局函数）
func NewAuthService() (*AuthService, error) {
	if dir := os.Getenv("KIRO_ADMIN_DATA_DIR"); dir != "" {
		if len(os.Getenv("KIRO_ADMIN_SECRET")) < 32 {
			return nil, fmt.Errorf("KIRO_ADMIN_SECRET must contain at least 32 characters")
		}
		var bootstrap []AuthConfig
		if _, err := os.Stat(filepath.Join(dir, "accounts.json")); os.IsNotExist(err) {
			raw := os.Getenv("KIRO_AUTH_TOKEN")
			if content, e := os.ReadFile(raw); e == nil {
				raw = string(content)
			}
			if raw != "" {
				var e error
				bootstrap, e = parseJSONConfig(raw)
				if e != nil {
					return nil, e
				}
			}
		}
		pool, err := NewAccountPool(dir, bootstrap, 10, nil)
		if err != nil {
			return nil, err
		}
		return &AuthService{managed: pool}, nil
	}
	logger.Info("创建AuthService实例")

	// 加载配置
	configs, err := loadConfigs()
	if err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}

	if len(configs) == 0 {
		return nil, fmt.Errorf("未找到有效的token配置")
	}

	// 创建token管理器
	tokenManager := NewTokenManager(configs)

	// 预热第一个可用token
	_, warmupErr := tokenManager.getBestToken()
	if warmupErr != nil {
		logger.Warn("token预热失败", logger.Err(warmupErr))
	}

	logger.Info("AuthService创建完成", logger.Int("config_count", len(configs)))

	return &AuthService{
		tokenManager: tokenManager,
		configs:      configs,
	}, nil
}

func (as *AuthService) ManagedPool() *AccountPool          { return as.managed }
func NewManagedAuthService(pool *AccountPool) *AuthService { return &AuthService{managed: pool} }

// GetToken 获取可用的token
func (as *AuthService) GetToken() (types.TokenInfo, error) {
	if as.tokenManager == nil {
		return types.TokenInfo{}, fmt.Errorf("token管理器未初始化")
	}
	return as.tokenManager.getBestToken()
}

// GetTokenWithUsage 获取可用的token（包含使用信息）
func (as *AuthService) GetTokenWithUsage() (*types.TokenWithUsage, error) {
	if as.tokenManager == nil {
		return nil, fmt.Errorf("token管理器未初始化")
	}
	return as.tokenManager.GetBestTokenWithUsage()
}

// GetTokenManager 获取底层的TokenManager（用于高级操作）
func (as *AuthService) GetTokenManager() *TokenManager {
	return as.tokenManager
}

// GetConfigs 获取认证配置
func (as *AuthService) GetConfigs() []AuthConfig {
	return as.configs
}
