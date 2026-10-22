package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kiro2api/types"
	"kiro2api/utils"
)

// Only this on-disk document contains credentials. HTTP handlers return AccountView.
type managedAccount struct {
	ID             string                  `json:"id"`
	Name           string                  `json:"name"`
	Email          string                  `json:"email"`
	Credentials    AuthConfig              `json:"credentials"`
	Concurrency    int                     `json:"concurrency"`
	ProxyID        string                  `json:"proxy_id,omitempty"`        // empty means a direct connection
	DisabledReason string                  `json:"disabled_reason,omitempty"` // set when the pool disabled the account itself
	Version        int                     `json:"version"`
	CreatedAt      time.Time               `json:"created_at"`
	DeletedAt      *time.Time              `json:"deleted_at,omitempty"`
	Usage          *CreditSnapshot         `json:"usage,omitempty"`
	Totals         RequestStats            `json:"totals"`
	Daily          map[string]RequestStats `json:"daily"`
	LastUsed       *time.Time              `json:"last_used,omitempty"`
	LastModel      string                  `json:"last_model,omitempty"`
	LastStatus     int                     `json:"last_status,omitempty"`
}

type managedDocument struct {
	Version   int              `json:"version"`
	StartedAt time.Time        `json:"started_at"`
	Accounts  []managedAccount `json:"accounts"`
	// SOCKS5 egress proxies; older binaries ignore this field and connect directly.
	Proxies []managedProxy `json:"proxies,omitempty"`
	// Pool-wide hourly/daily aggregates; older binaries ignore this field.
	Metrics *poolMetrics `json:"metrics,omitempty"`
	// Account types served first, in this order; see planType. Older binaries ignore it.
	PlanOrder []string `json:"plan_order,omitempty"`
	// Client API keys created in the admin page; see apikeys.go.
	APIKeys []managedAPIKey `json:"api_keys,omitempty"`
}

type CreditSnapshot struct {
	Email     string     `json:"email"`
	Plan      string     `json:"plan"`
	Limit     float64    `json:"limit"`
	Used      float64    `json:"used"`
	Remaining float64    `json:"remaining"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	CheckedAt time.Time  `json:"checked_at"`
}

type RequestStats struct {
	Requests        int64 `json:"requests"`
	Succeeded       int64 `json:"succeeded"`
	Failed          int64 `json:"failed"`
	Canceled        int64 `json:"canceled"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
}

type AccountView struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Email          string          `json:"email"`
	Auth           string          `json:"auth"`
	PlanType       string          `json:"plan_type"`
	Enabled        bool            `json:"enabled"`
	Concurrency    int             `json:"concurrency"`
	ProxyID        string          `json:"proxy_id,omitempty"`
	DisabledReason string          `json:"disabled_reason,omitempty"`
	Active         int             `json:"active"`
	Version        int             `json:"version"`
	Status         string          `json:"status"`
	Error          string          `json:"error,omitempty"`
	Refreshing     bool            `json:"refreshing"`
	CreatedAt      time.Time       `json:"created_at"`
	DeletedAt      *time.Time      `json:"deleted_at,omitempty"`
	Usage          *CreditSnapshot `json:"usage,omitempty"`
	TokenExpiresAt *time.Time      `json:"token_expires_at,omitempty"`
	Totals         RequestStats    `json:"totals"`
	Today          RequestStats    `json:"today"`
	LastUsed       *time.Time      `json:"last_used,omitempty"`
	LastModel      string          `json:"last_model,omitempty"`
	LastStatus     int             `json:"last_status,omitempty"`
}

// ImportAccount accepts both the existing Social/IdC format and Kiro export JSON.
type ImportAccount struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	AuthConfig
	Concurrency int `json:"concurrency"`
}

type AccountPatch struct {
	Version      int     `json:"version"`
	Name         *string `json:"name,omitempty"`
	Enabled      *bool   `json:"enabled,omitempty"`
	Concurrency  *int    `json:"concurrency,omitempty"`
	RefreshToken *string `json:"refreshToken,omitempty"`
	// Empty string switches the account to a direct connection.
	ProxyID *string `json:"proxy_id,omitempty"`
}

// AccountRestore re-attaches credentials to a deleted account. Deletion wipes them,
// so they must be supplied again. Email, when given, must match the record.
type AccountRestore struct {
	Version  int    `json:"version"`
	Email    string `json:"email,omitempty"`
	Provider string `json:"provider,omitempty"`
	AuthConfig
}

// planType reduces an official subscription title such as "KIRO STUDENT" to the
// account type used for tags and allocation order. It is empty until the first
// official usage check.
func planType(u *CreditSnapshot) string {
	if u == nil {
		return ""
	}
	t := strings.ToUpper(strings.TrimSpace(u.Plan))
	return strings.TrimSpace(strings.TrimPrefix(t, "KIRO"))
}

func normalizePlanOrder(in []string) ([]string, error) {
	if len(in) > 20 {
		return nil, errors.New("最多设置 20 个账号类型")
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.ToUpper(strings.TrimSpace(t))
		if t == "" || len(t) > 40 || strings.ContainsAny(t, "\r\n\t\x00") {
			return nil, errors.New("账号类型名称无效")
		}
		if seen[t] {
			return nil, errors.New("账号类型重复")
		}
		seen[t] = true
		out = append(out, t)
	}
	return out, nil
}

var ErrAccountMissing = errors.New("账号不存在")
var ErrAccountConflict = errors.New("账号已被更新，请刷新页面后再操作")

func normalizeImport(in ImportAccount, defaultLimit int) (ImportAccount, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.TrimSpace(in.Email)
	if strings.HasPrefix(in.Email, "[") {
		if n := strings.Index(in.Email, "]("); n > 1 {
			in.Email = in.Email[1:n]
		}
	}
	if len(in.Name) > 120 || strings.ContainsAny(in.Name, "\r\n\x00") {
		return in, errors.New("账号名称最多 120 字节且不能包含换行")
	}
	if in.Email != "" {
		a, e := mail.ParseAddress(in.Email)
		if e != nil || a.Address != in.Email || len(in.Email) > 254 {
			return in, errors.New("邮箱格式无效")
		}
	}
	in.RefreshToken = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(in.RefreshToken, `\_`, "_"), `\@`, "@"))
	if len(in.RefreshToken) < 20 || len(in.RefreshToken) > 16384 || strings.ContainsAny(in.RefreshToken, " \t\r\n\x00") {
		return in, errors.New("refreshToken 格式无效")
	}
	if in.AuthType == "" {
		switch strings.ToLower(in.Provider) {
		case "", "github", "google", "social":
			in.AuthType = AuthMethodSocial
		case "idc", "builderid", "builder_id", "builder-id":
			// Kiro exports AWS Builder ID credentials with provider=BuilderId.
			// They use the same OIDC refresh flow as the IdC form accepted by
			// the bridge, so treat the export spelling as an IdC alias.
			in.AuthType = AuthMethodIdC
		default:
			return in, errors.New("支持 Github、Google、Social、BuilderId 或 IdC 账号")
		}
	}
	if in.AuthType != AuthMethodSocial && in.AuthType != AuthMethodIdC {
		return in, errors.New("认证类型必须是 Social 或 IdC")
	}
	if in.AuthType == AuthMethodIdC && (in.ClientID == "" || in.ClientSecret == "") {
		return in, errors.New("IdC 账号需要 clientId 和 clientSecret")
	}
	if len(in.ClientID) > 4096 || len(in.ClientSecret) > 16384 {
		return in, errors.New("IdC 凭据过长")
	}
	if in.Concurrency == 0 {
		in.Concurrency = defaultLimit
	}
	if in.Concurrency < 1 || in.Concurrency > 100 {
		return in, errors.New("账号并发上限必须在 1–100 之间")
	}
	return in, nil
}

func newManagedAccount(in ImportAccount, defaultLimit int) (managedAccount, error) {
	in, err := normalizeImport(in, defaultLimit)
	if err != nil {
		return managedAccount{}, err
	}
	id := utils.GenerateUUID()
	if in.Name == "" {
		in.Name = in.Email
	}
	if in.Name == "" {
		in.Name = "Kiro " + id[:8]
	}
	return managedAccount{ID: id, Name: in.Name, Email: in.Email, Credentials: in.AuthConfig, Concurrency: in.Concurrency, Version: 1, CreatedAt: time.Now().UTC(), Daily: map[string]RequestStats{}}, nil
}

func cloneDocument(d managedDocument) managedDocument {
	b, _ := json.Marshal(d)
	var out managedDocument
	_ = json.Unmarshal(b, &out)
	return out
}

func writeAccountDocument(path string, d managedDocument) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return atomicPrivateFile(path, append(b, '\n'))
}

func atomicPrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".accounts-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, e := os.Open(filepath.Dir(path)); e == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func loadAccountDocument(dir string, bootstrap []AuthConfig, defaultLimit int) (managedDocument, error) {
	var d managedDocument
	if err := os.MkdirAll(dir, 0700); err != nil {
		return d, err
	}
	p := filepath.Join(dir, "accounts.json")
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		d = managedDocument{Version: 1, StartedAt: time.Now().UTC(), Accounts: []managedAccount{}}
		seen := map[string]bool{}
		for _, cfg := range bootstrap {
			if seen[cfg.RefreshToken] {
				continue
			}
			a, e := newManagedAccount(ImportAccount{AuthConfig: cfg}, defaultLimit)
			if e != nil {
				return d, e
			}
			seen[cfg.RefreshToken] = true
			d.Accounts = append(d.Accounts, a)
		}
		return d, writeAccountDocument(p, d)
	}
	if err != nil {
		return d, err
	}
	if len(b) > 16<<20 {
		return d, errors.New("账号文件过大")
	}
	if err = json.Unmarshal(b, &d); err != nil {
		return d, errors.New("账号文件格式错误，请从备份恢复；未覆盖现有文件")
	}
	if d.Version != 1 || d.StartedAt.IsZero() {
		return d, errors.New("不支持的账号文件版本")
	}
	order, err := normalizePlanOrder(d.PlanOrder)
	if err != nil {
		return d, errors.New("账号类型顺序无效")
	}
	d.PlanOrder = order
	if err = validateKeys(&d, time.Now()); err != nil {
		return d, err
	}
	proxies := map[string]bool{}
	for i := range d.Proxies {
		x := &d.Proxies[i]
		if x.ID == "" || proxies[x.ID] || normalizeProxy(x) != nil {
			return d, fmt.Errorf("代理配置无效: %s", x.ID)
		}
		proxies[x.ID] = true
	}
	ids := map[string]bool{}
	tokens := map[string]bool{}
	for i := range d.Accounts {
		a := &d.Accounts[i]
		if a.ID == "" || ids[a.ID] {
			return d, errors.New("账号 ID 缺失或重复")
		}
		ids[a.ID] = true
		if a.DeletedAt != nil {
			a.Credentials = AuthConfig{}
			continue
		}
		if _, e := normalizeImport(ImportAccount{AuthConfig: a.Credentials, Name: a.Name, Email: a.Email, Concurrency: a.Concurrency}, defaultLimit); e != nil {
			return d, fmt.Errorf("账号配置无效: %s", a.ID)
		}
		// Refuse to start rather than silently send this account's traffic direct.
		if a.ProxyID != "" && !proxies[a.ProxyID] {
			return d, fmt.Errorf("账号引用的代理不存在: %s", a.ID)
		}
		if tokens[a.Credentials.RefreshToken] {
			return d, errors.New("账号凭据重复")
		}
		tokens[a.Credentials.RefreshToken] = true
		if a.Daily == nil {
			a.Daily = map[string]RequestStats{}
		}
	}
	return d, nil
}

func tokenWithSnapshot(token types.TokenInfo, usage *CreditSnapshot) *types.TokenWithUsage {
	result := &types.TokenWithUsage{TokenInfo: token}
	if usage != nil {
		result.AvailableCount = usage.Remaining
		result.LastUsageCheck = usage.CheckedAt
		result.UserEmail = usage.Email
	}
	return result
}
