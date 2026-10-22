package utils

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/proxy"
	"kiro2api/config"
)

var (
	// SharedHTTPClient 共享的HTTP客户端实例，优化了连接池和性能配置
	SharedHTTPClient *http.Client
)

// ProxyHandshakeTimeout 连接代理并完成 SOCKS5 协商的最长时间，避免失联代理长期占用账号并发
const ProxyHandshakeTimeout = 15 * time.Second

// SOCKS5Proxy 账号出口代理。目标域名交给代理解析（等同 socks5h），不在本机解析。
type SOCKS5Proxy struct {
	Address  string
	Username string
	Password string
}

func init() {
	// 检查TLS配置并记录日志
	if shouldSkipTLSVerify() {
		os.Stderr.WriteString("[WARNING] TLS证书验证已禁用 - 仅适用于开发/调试环境\n")
	}

	// 创建统一的HTTP客户端
	SharedHTTPClient = NewHTTPClient(nil)
}

// NewHTTPClient 按统一的连接池与TLS配置创建客户端。
// socks 为空时直连（沿用环境变量中的代理设置）；否则所有连接都经由该 SOCKS5 代理。
func NewHTTPClient(socks *SOCKS5Proxy) *http.Client {
	// 连接建立配置
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: config.HTTPClientKeepAlive,
		DualStack: true,
	}
	transport := &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: dialer.DialContext,

		// TLS配置
		TLSHandshakeTimeout: config.HTTPClientTLSHandshakeTimeout,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: shouldSkipTLSVerify(),
			MinVersion:         tls.VersionTLS12,
			MaxVersion:         tls.VersionTLS13,
			CipherSuites: []uint16{
				tls.TLS_AES_256_GCM_SHA384,
				tls.TLS_CHACHA20_POLY1305_SHA256,
				tls.TLS_AES_128_GCM_SHA256,
			},
		},

		// HTTP配置
		ForceAttemptHTTP2:  false,
		DisableCompression: false,
	}
	if socks != nil {
		var auth *proxy.Auth
		if socks.Username != "" {
			auth = &proxy.Auth{User: socks.Username, Password: socks.Password}
		}
		// proxy.SOCKS5 只构造拨号器，不会返回错误
		d, _ := proxy.SOCKS5("tcp", socks.Address, auth, dialer)
		socksDialer := d.(proxy.ContextDialer)
		// 显式代理优先于环境变量，且不能在代理失败时回退直连
		transport.Proxy = nil
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, ProxyHandshakeTimeout)
			defer cancel()
			return socksDialer.DialContext(ctx, network, address)
		}
	}
	return &http.Client{Transport: transport}
}

// shouldSkipTLSVerify 根据GIN_MODE决定是否跳过TLS证书验证
func shouldSkipTLSVerify() bool {
	return os.Getenv("GIN_MODE") == "debug"
}

type httpClientKey struct{}

// WithHTTPClient 让携带该 ctx 的 DoRequest 请求改用指定客户端（例如账号绑定的代理）。
// client 为空时原样返回 ctx，请求继续使用共享客户端。
func WithHTTPClient(ctx context.Context, client *http.Client) context.Context {
	if client == nil {
		return ctx
	}
	return context.WithValue(ctx, httpClientKey{}, client)
}

// DoRequest 执行HTTP请求；请求上下文携带账号客户端时优先使用它
func DoRequest(req *http.Request) (*http.Response, error) {
	if client, ok := req.Context().Value(httpClientKey{}).(*http.Client); ok {
		return client.Do(req)
	}
	return SharedHTTPClient.Do(req)
}

// IsProxyError 判断错误是否发生在连接或协商代理阶段，而不是代理后面的上游服务。
// 客户端主动取消不算代理故障。
func IsProxyError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var op *net.OpError
	return errors.As(err, &op) && (op.Op == "proxyconnect" || strings.HasPrefix(op.Op, "socks"))
}

// ProxyErrorMessage 把代理错误转换为可以展示的说明，不包含代理地址或凭据
func ProxyErrorMessage(err error) string {
	msg := err.Error()
	var timeout interface{ Timeout() bool }
	switch {
	case strings.Contains(msg, "unknown error "):
		// SOCKS5 服务端的应答码，例如 host unreachable、connection not allowed by ruleset
		reply := msg[strings.LastIndex(msg, "unknown error ")+len("unknown error "):]
		return "代理无法连接 Kiro 上游（" + reply + "）"
	case strings.Contains(msg, "authentication failed"), strings.Contains(msg, "no acceptable authentication methods"), strings.Contains(msg, "invalid username/password"):
		return "代理认证失败，请检查用户名和密码"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "连接代理超时"
	case strings.Contains(msg, "no such host"):
		return "无法解析代理主机"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "actively refused"):
		return "代理拒绝连接，请检查地址和端口"
	case strings.Contains(msg, "unexpected protocol version"):
		return "该地址不是 SOCKS5 代理"
	}
	return "代理连接失败"
}
