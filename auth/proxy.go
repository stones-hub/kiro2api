package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kiro2api/logger"
	"kiro2api/utils"
)

// SOCKS5 egress proxies. Credentials stay in accounts.json; the admin API only
// reports whether a password is set.
type managedProxy struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Username  string    `json:"username,omitempty"`
	Password  string    `json:"password,omitempty"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

type ProxyView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	Username    string    `json:"username,omitempty"`
	HasPassword bool      `json:"has_password"`
	Accounts    int       `json:"accounts"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
}

// ProxyInput creates or patches a proxy. Nil fields keep their value. AccountIDs,
// when present, is the complete set of accounts that should use this proxy.
type ProxyInput struct {
	Version    int       `json:"version"`
	Name       *string   `json:"name,omitempty"`
	Host       *string   `json:"host,omitempty"`
	Port       *int      `json:"port,omitempty"`
	Username   *string   `json:"username,omitempty"`
	Password   *string   `json:"password,omitempty"`
	AccountIDs *[]string `json:"account_ids,omitempty"`
}

type proxyClient struct {
	key    string
	client *http.Client
}

const maxProxies = 100

var ErrProxyMissing = errors.New("代理不存在")
var ErrProxyConflict = errors.New("代理已被更新，请刷新页面后再操作")
var ErrProxyInUse = errors.New("代理仍在使用中")

// proxyProbeURL is only contacted to prove the proxy can reach the Kiro upstream.
var proxyProbeURL = "https://q.us-east-1.amazonaws.com/"

func validHostname(h string) bool {
	if h == "" || len(h) > 253 || h[0] == '-' || h[0] == '.' {
		return false
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func normalizeProxy(x *managedProxy) error {
	x.Name = strings.TrimSpace(x.Name)
	x.Host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(x.Host), "["), "]")
	x.Username = strings.TrimSpace(x.Username)
	if len(x.Name) > 120 || strings.ContainsAny(x.Name, "\r\n\x00") {
		return errors.New("代理名称最多 120 字节且不能包含换行")
	}
	if net.ParseIP(x.Host) == nil && !validHostname(x.Host) {
		return errors.New("代理主机必须是域名或 IP，不含协议、端口和路径")
	}
	if x.Port < 1 || x.Port > 65535 {
		return errors.New("代理端口必须在 1–65535 之间")
	}
	// RFC 1929 limits both fields to 255 bytes.
	if len(x.Username) > 255 || len(x.Password) > 255 || strings.ContainsAny(x.Username+x.Password, "\r\n\x00") {
		return errors.New("代理用户名和密码各最多 255 字节且不能包含换行")
	}
	if x.Username == "" && x.Password != "" {
		return errors.New("填写代理密码时需要同时填写用户名")
	}
	return nil
}

func (x managedProxy) address() string { return net.JoinHostPort(x.Host, strconv.Itoa(x.Port)) }

func (x managedProxy) socks() *utils.SOCKS5Proxy {
	return &utils.SOCKS5Proxy{Address: x.address(), Username: x.Username, Password: x.Password}
}

// routeKey changes whenever the connection a proxy produces would change.
func (x managedProxy) routeKey() string {
	return strings.Join([]string{x.address(), x.Username, x.Password}, "\x00")
}

func proxyIndex(d *managedDocument, id string) int {
	for i := range d.Proxies {
		if d.Proxies[i].ID == id {
			return i
		}
	}
	return -1
}

// syncClientsLocked keeps one connection pool per proxy and rebuilds it only when
// the address or credentials change. In-flight requests keep the client they hold.
func (p *AccountPool) syncClientsLocked() {
	next := make(map[string]proxyClient, len(p.doc.Proxies))
	for _, x := range p.doc.Proxies {
		key := x.routeKey()
		if old, ok := p.clients[x.ID]; ok && old.key == key {
			next[x.ID] = old
			continue
		}
		next[x.ID] = proxyClient{key: key, client: utils.NewHTTPClient(x.socks())}
	}
	for id, old := range p.clients {
		if cur, ok := next[id]; !ok || cur.client != old.client {
			old.client.CloseIdleConnections()
		}
	}
	p.clients = next
}

// clientLocked returns the HTTP client of an account's proxy; nil means direct.
// ok is false when the account names a proxy that does not exist, which must
// never fall back to a direct connection.
func (p *AccountPool) clientLocked(a *managedAccount) (*http.Client, bool) {
	if a.ProxyID == "" {
		return nil, true
	}
	c, ok := p.clients[a.ProxyID]
	return c.client, ok
}

// selectProxyAccounts makes exactly ids use proxy id; accounts leaving it become
// direct. It returns the accounts whose route changed.
func selectProxyAccounts(next *managedDocument, id string, ids []string) ([]string, error) {
	if len(ids) > 500 {
		return nil, errors.New("最多勾选 500 个账号")
	}
	live := map[string]bool{}
	for _, a := range next.Accounts {
		if a.DeletedAt == nil {
			live[a.ID] = true
		}
	}
	want := map[string]bool{}
	for _, aid := range ids {
		if !live[aid] {
			return nil, ErrAccountMissing
		}
		want[aid] = true
	}
	changed := []string{}
	for i := range next.Accounts {
		a := &next.Accounts[i]
		if a.DeletedAt != nil {
			continue
		}
		route := a.ProxyID
		if want[a.ID] {
			route = id
		} else if a.ProxyID == id {
			route = ""
		}
		if route != a.ProxyID {
			a.ProxyID = route
			a.Version++
			changed = append(changed, a.ID)
		}
	}
	return changed, nil
}

// rerouteLocked re-checks accounts right away so a new route is verified before
// the next regular refresh.
func (p *AccountPool) rerouteLocked(ids []string) {
	for _, id := range ids {
		i := p.indexLocked(id)
		rt := p.runtime[id]
		if i < 0 || rt == nil {
			continue
		}
		rt.nextRefresh = time.Time{}
		if !p.doc.Accounts[i].Credentials.Disabled {
			p.scheduleLocked(id)
		}
	}
}

func applyProxyInput(x *managedProxy, in ProxyInput) {
	if in.Name != nil {
		x.Name = *in.Name
	}
	if in.Host != nil {
		x.Host = *in.Host
	}
	if in.Port != nil {
		x.Port = *in.Port
	}
	if in.Username != nil {
		x.Username = *in.Username
		// Removing the username also removes the password it belonged to.
		if strings.TrimSpace(x.Username) == "" {
			x.Password = ""
		}
	}
	if in.Password != nil {
		x.Password = *in.Password
	}
}

func (p *AccountPool) CreateProxy(in ProxyInput) (string, error) {
	x := managedProxy{ID: utils.GenerateUUID(), Version: 1, CreatedAt: time.Now().UTC()}
	applyProxyInput(&x, in)
	if err := normalizeProxy(&x); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.doc.Proxies) >= maxProxies {
		return "", fmt.Errorf("最多配置 %d 个代理", maxProxies)
	}
	next := cloneDocument(p.doc)
	next.Proxies = append(next.Proxies, x)
	changed := []string{}
	if in.AccountIDs != nil {
		var err error
		if changed, err = selectProxyAccounts(&next, x.ID, *in.AccountIDs); err != nil {
			return "", err
		}
	}
	if err := p.commitLocked(next); err != nil {
		return "", err
	}
	p.rerouteLocked(changed)
	return x.ID, nil
}

func (p *AccountPool) UpdateProxy(id string, in ProxyInput) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := proxyIndex(&p.doc, id)
	if i < 0 {
		return ErrProxyMissing
	}
	if in.Version != p.doc.Proxies[i].Version {
		return ErrProxyConflict
	}
	next := cloneDocument(p.doc)
	x := &next.Proxies[i]
	before := x.routeKey()
	applyProxyInput(x, in)
	if err := normalizeProxy(x); err != nil {
		return err
	}
	x.Version++
	changed := []string{}
	if in.AccountIDs != nil {
		var err error
		if changed, err = selectProxyAccounts(&next, id, *in.AccountIDs); err != nil {
			return err
		}
	}
	if x.routeKey() != before {
		for _, a := range next.Accounts {
			if a.DeletedAt == nil && a.ProxyID == id {
				changed = append(changed, a.ID)
			}
		}
	}
	if err := p.commitLocked(next); err != nil {
		return err
	}
	p.rerouteLocked(changed)
	return nil
}

// DeleteProxy refuses while accounts use the proxy: silently switching them to a
// direct connection would expose the server's own address.
func (p *AccountPool) DeleteProxy(id string, version int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := proxyIndex(&p.doc, id)
	if i < 0 {
		return ErrProxyMissing
	}
	if version != p.doc.Proxies[i].Version {
		return ErrProxyConflict
	}
	used := 0
	for _, a := range p.doc.Accounts {
		if a.DeletedAt == nil && a.ProxyID == id {
			used++
		}
	}
	if used > 0 {
		return fmt.Errorf("%w：%d 个账号正在使用，请先取消勾选或改用其他代理", ErrProxyInUse, used)
	}
	next := cloneDocument(p.doc)
	next.Proxies = append(next.Proxies[:i], next.Proxies[i+1:]...)
	return p.commitLocked(next)
}

func (p *AccountPool) proxyViewsLocked() []ProxyView {
	counts := map[string]int{}
	for _, a := range p.doc.Accounts {
		if a.DeletedAt == nil && a.ProxyID != "" {
			counts[a.ProxyID]++
		}
	}
	out := make([]ProxyView, 0, len(p.doc.Proxies))
	for _, x := range p.doc.Proxies {
		out = append(out, ProxyView{ID: x.ID, Name: x.Name, Host: x.Host, Port: x.Port, Username: x.Username, HasPassword: x.Password != "", Accounts: counts[x.ID], Version: x.Version, CreatedAt: x.CreatedAt})
	}
	return out
}

func (p *AccountPool) Proxies() []ProxyView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.proxyViewsLocked()
}

// TestProxy opens a connection to the Kiro upstream through the proxy. Any HTTP
// answer proves the route works; the returned error text is safe to show.
func (p *AccountPool) TestProxy(ctx context.Context, id string) (time.Duration, error) {
	p.mu.Lock()
	c, ok := p.clients[id]
	p.mu.Unlock()
	if !ok {
		return 0, ErrProxyMissing
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, proxyProbeURL, nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		logger.Warn("代理测试失败", logger.String("proxy_id", id), logger.Err(err))
		if utils.IsProxyError(err) {
			return 0, errors.New(utils.ProxyErrorMessage(err))
		}
		return 0, errors.New("代理已连接，但无法访问 Kiro 上游")
	}
	resp.Body.Close()
	return time.Since(start), nil
}
