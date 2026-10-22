package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
)

type adminOptions struct {
	Secret    string
	Origins   []string
	StaticDir string
}

func NewAdminRouter(pool *auth.AccountPool, secret string, origins []string, staticDir string) (http.Handler, error) {
	if pool == nil || len(secret) < 32 {
		return nil, errors.New("account management requires an independent secret")
	}
	allowed := map[string]bool{}
	for _, s := range origins {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("invalid admin origin")
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
			return nil, errors.New("admin origins must use HTTPS")
		}
		allowed[u.Scheme+"://"+u.Host] = true
	}
	if len(allowed) == 0 {
		return nil, errors.New("KIRO_ADMIN_ORIGINS is required")
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	csrf := hex.EncodeToString(nonce[:])
	r := gin.New()
	r.Use(gin.Recovery())
	var rateMu sync.Mutex
	window := time.Now()
	writes := 0
	r.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if subtle.ConstantTimeCompare([]byte(c.GetHeader("X-Kiro-Admin-Token")), []byte(secret)) != 1 {
			c.AbortWithStatusJSON(401, gin.H{"error": "需要管理员认证"})
			return
		}
		if origin := c.GetHeader("Origin"); origin != "" && !allowed[origin] {
			c.AbortWithStatusJSON(403, gin.H{"error": "请求来源不允许"})
			return
		}
		if site := c.GetHeader("Sec-Fetch-Site"); site == "cross-site" {
			c.AbortWithStatusJSON(403, gin.H{"error": "请求来源不允许"})
			return
		}
		if c.Request.Method != "GET" && c.Request.Method != "HEAD" {
			if !allowed[c.GetHeader("Origin")] || subtle.ConstantTimeCompare([]byte(c.GetHeader("X-CSRF-Token")), []byte(csrf)) != 1 {
				c.AbortWithStatusJSON(403, gin.H{"error": "安全校验已过期，请刷新页面"})
				return
			}
			kind, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
			if err != nil || kind != "application/json" {
				c.AbortWithStatusJSON(415, gin.H{"error": "请提交 JSON 格式"})
				return
			}
			rateMu.Lock()
			if time.Since(window) > time.Minute {
				window = time.Now()
				writes = 0
			}
			writes++
			limited := writes > 120
			rateMu.Unlock()
			if limited {
				c.AbortWithStatusJSON(429, gin.H{"error": "操作过于频繁，请稍后再试"})
				return
			}
		}
		c.Next()
	})
	readJSON := func(c *gin.Context, dst any) bool {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		d := json.NewDecoder(c.Request.Body)
		d.DisallowUnknownFields()
		if err := d.Decode(dst); err != nil {
			c.JSON(400, gin.H{"error": "JSON 格式或字段无效"})
			return false
		}
		if d.Decode(new(any)) != io.EOF {
			c.JSON(400, gin.H{"error": "请只提交一个 JSON 对象"})
			return false
		}
		return true
	}
	fail := func(c *gin.Context, err error) {
		status := 400
		if errors.Is(err, auth.ErrAccountConflict) || errors.Is(err, auth.ErrProxyConflict) || errors.Is(err, auth.ErrProxyInUse) || errors.Is(err, auth.ErrKeyConflict) {
			status = 409
		}
		if errors.Is(err, auth.ErrAccountMissing) || errors.Is(err, auth.ErrProxyMissing) || errors.Is(err, auth.ErrKeyMissing) {
			status = 404
		}
		c.JSON(status, gin.H{"error": err.Error()})
	}
	r.GET("/api/status", func(c *gin.Context) {
		c.JSON(200, gin.H{"version": "20261021-docsearch-1", "csrf_token": csrf, "cache_enabled": os.Getenv("KIRO_LOCAL_CACHE_DISCOUNT") == "true", "cache_percent": os.Getenv("KIRO_LOCAL_CACHE_MAX_PERCENT"), "cache_fixed_on_hit": os.Getenv("KIRO_LOCAL_CACHE_FIXED_PERCENT_ON_HIT") == "true"})
	})
	r.GET("/api/accounts", func(c *gin.Context) { c.JSON(200, pool.View()) })
	r.GET("/api/metrics", func(c *gin.Context) {
		key := c.Query("key")
		if key == "" {
			c.JSON(200, pool.Metrics())
			return
		}
		m, err := pool.MetricsForKey(key)
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, m)
	})
	registerModelRoutes(r, pool, readJSON, fail)
	r.GET("/api/usage", func(c *gin.Context) {
		atoi := func(k string) int {
			n, _ := strconv.Atoi(c.Query(k))
			return n
		}
		page, err := pool.UsageLogs(auth.UsageQuery{Date: c.Query("date"), KeyID: c.Query("key"), AccountID: c.Query("account"),
			Model: c.Query("model"), Result: c.Query("result"), Limit: atoi("limit"), Offset: atoi("offset")})
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"page": page, "dates": pool.UsageDates()})
	})
	r.GET("/api/keys", func(c *gin.Context) { c.JSON(200, gin.H{"keys": pool.APIKeys()}) })
	r.POST("/api/keys", func(c *gin.Context) {
		var in struct {
			Name string `json:"name"`
		}
		if !readJSON(c, &in) {
			return
		}
		id, key, err := pool.CreateAPIKey(in.Name)
		if err != nil {
			fail(c, err)
			return
		}
		// The only time the key is returned; the server keeps just its hash.
		c.JSON(201, gin.H{"id": id, "key": key})
	})
	r.PATCH("/api/keys/:id", func(c *gin.Context) {
		var in auth.APIKeyPatch
		if !readJSON(c, &in) {
			return
		}
		if err := pool.UpdateAPIKey(c.Param("id"), in); err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	r.DELETE("/api/keys/:id", func(c *gin.Context) {
		var in struct {
			Version int `json:"version"`
		}
		if !readJSON(c, &in) {
			return
		}
		if err := pool.DeleteAPIKey(c.Param("id"), in.Version); err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	r.POST("/api/accounts", func(c *gin.Context) {
		var in struct {
			Accounts []auth.ImportAccount `json:"accounts"`
		}
		if !readJSON(c, &in) {
			return
		}
		ids, duplicates, err := pool.Import(in.Accounts)
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(201, gin.H{"added": len(ids), "duplicates": duplicates})
	})
	r.POST("/api/accounts/batch", func(c *gin.Context) {
		var in auth.AccountBatch
		if !readJSON(c, &in) {
			return
		}
		n, err := pool.Batch(in)
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"changed": n})
	})
	r.PATCH("/api/accounts/:id", func(c *gin.Context) {
		var in auth.AccountPatch
		if !readJSON(c, &in) {
			return
		}
		if err := pool.Update(c.Param("id"), in); err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	r.DELETE("/api/accounts/:id", func(c *gin.Context) {
		var in struct {
			Version int `json:"version"`
		}
		if !readJSON(c, &in) {
			return
		}
		if err := pool.Delete(c.Param("id"), in.Version); err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	r.POST("/api/accounts/:id/restore", func(c *gin.Context) {
		var in auth.AccountRestore
		if !readJSON(c, &in) {
			return
		}
		if err := pool.Restore(c.Param("id"), in); err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	refresh := func(c *gin.Context) {
		var in struct{}
		if !readJSON(c, &in) {
			return
		}
		n, err := pool.Refresh(c.Param("id"))
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(202, gin.H{"scheduled": n})
	}
	r.PUT("/api/plan-order", func(c *gin.Context) {
		var in struct {
			Order []string `json:"order"`
		}
		if !readJSON(c, &in) {
			return
		}
		if err := pool.SetPlanOrder(in.Order); err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	r.GET("/api/proxies", func(c *gin.Context) { c.JSON(200, gin.H{"proxies": pool.Proxies()}) })
	r.POST("/api/proxies", func(c *gin.Context) {
		var in auth.ProxyInput
		if !readJSON(c, &in) {
			return
		}
		id, err := pool.CreateProxy(in)
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(201, gin.H{"id": id})
	})
	r.PATCH("/api/proxies/:id", func(c *gin.Context) {
		var in auth.ProxyInput
		if !readJSON(c, &in) {
			return
		}
		if err := pool.UpdateProxy(c.Param("id"), in); err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	r.DELETE("/api/proxies/:id", func(c *gin.Context) {
		var in struct {
			Version int `json:"version"`
		}
		if !readJSON(c, &in) {
			return
		}
		if err := pool.DeleteProxy(c.Param("id"), in.Version); err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})
	r.POST("/api/proxies/:id/test", func(c *gin.Context) {
		var in struct{}
		if !readJSON(c, &in) {
			return
		}
		latency, err := pool.TestProxy(c.Request.Context(), c.Param("id"))
		if errors.Is(err, auth.ErrProxyMissing) {
			fail(c, err)
			return
		}
		if err != nil {
			c.JSON(200, gin.H{"ok": false, "error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "latency_ms": latency.Milliseconds()})
	})
	r.POST("/api/accounts/:id/refresh", refresh)
	r.POST("/api/refresh", refresh)
	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.GET("/", func(c *gin.Context) { c.File(staticDir + "/index.html") })
	r.Static("/assets", staticDir+"/assets")
	r.NoRoute(func(c *gin.Context) { c.JSON(404, gin.H{"error": "页面不存在"}) })
	return r, nil
}

func adminServerFromEnv(service *auth.AuthService) (*http.Server, error) {
	if service.ManagedPool() == nil {
		return nil, nil
	}
	handler, err := NewAdminRouter(service.ManagedPool(), os.Getenv("KIRO_ADMIN_SECRET"), strings.Split(os.Getenv("KIRO_ADMIN_ORIGINS"), ","), "./static/admin")
	if err != nil {
		return nil, err
	}
	bind := os.Getenv("KIRO_ADMIN_BIND")
	if bind == "" {
		bind = "127.0.0.1:8081"
	}
	return &http.Server{Addr: bind, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}, nil
}
