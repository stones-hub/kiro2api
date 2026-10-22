package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"kiro2api/utils"
)

// ListModelsURL is Kiro's account-scoped model catalog; tests and the local preview point it elsewhere.
var ListModelsURL = "https://codewhisperer.us-east-1.amazonaws.com/ListAvailableModels"

var ErrNoModelAccount = errors.New("没有可用于检测的账号，请等待账号凭据刷新完成后重试")

type UpstreamModels struct {
	AccountID string            `json:"account_id"`
	Account   string            `json:"account"`
	Plan      string            `json:"plan"`
	Models    []json.RawMessage `json:"-"`
}

// FetchUpstreamModels reads ListAvailableModels with one account's credential
// and proxy. Without accountID it prefers a paid account in allocation order,
// since free plans may see fewer models. Exhausted accounts still qualify: the
// catalog call does not spend credits.
func (p *AccountPool) FetchUpstreamModels(ctx context.Context, accountID string) (UpstreamModels, error) {
	p.mu.Lock()
	now := time.Now()
	var pick *managedAccount
	found := accountID == ""
	for pass := 0; pass < 2 && pick == nil; pass++ {
		for _, i := range p.allocationOrderLocked() {
			a := &p.doc.Accounts[i]
			if a.DeletedAt != nil || (accountID != "" && a.ID != accountID) {
				continue
			}
			found = true
			rt := p.runtime[a.ID]
			if a.Credentials.Disabled && accountID == "" {
				continue
			}
			if rt.lastError != "" || rt.token.AccessToken == "" || !now.Before(rt.token.ExpiresAt) {
				continue
			}
			if pass == 0 && accountID == "" && (a.Usage == nil || isFreePlan(a.Usage)) {
				continue
			}
			pick = a
			break
		}
	}
	if !found {
		p.mu.Unlock()
		return UpstreamModels{}, ErrAccountMissing
	}
	if pick == nil {
		p.mu.Unlock()
		return UpstreamModels{}, ErrNoModelAccount
	}
	client, routed := p.clientLocked(pick)
	token := p.runtime[pick.ID].token
	out := UpstreamModels{AccountID: pick.ID, Account: pick.Email, Plan: planType(pick.Usage)}
	if out.Account == "" {
		out.Account = pick.Name
	}
	p.mu.Unlock()
	if !routed {
		return out, errors.New("账号代理不存在")
	}

	// Below the admin server's 30 s write timeout, so the page always gets an answer.
	ctx, cancel := context.WithTimeout(utils.WithHTTPClient(ctx, client), 20*time.Second)
	defer cancel()
	next := ""
	for page := 0; page < 10; page++ {
		q := url.Values{"origin": {"AI_EDITOR"}, "maxResults": {"50"}}
		if token.ProfileArn != "" {
			q.Set("profileArn", token.ProfileArn)
		}
		if next != "" {
			q.Set("nextToken", next)
		}
		var resp struct {
			Models    []json.RawMessage `json:"models"`
			NextToken string            `json:"nextToken"`
		}
		if err := fetchAccountJSON(ctx, "GET", ListModelsURL+"?"+q.Encode(), nil, token.AccessToken, &resp, "模型列表查询"); err != nil {
			return out, errors.New(safeAccountError(err))
		}
		out.Models = append(out.Models, resp.Models...)
		if next = resp.NextToken; next == "" {
			break
		}
	}
	if next != "" {
		return out, errors.New("模型列表分页过多，已停止读取")
	}
	if len(out.Models) == 0 {
		return out, errors.New("Kiro 没有返回任何模型")
	}
	return out, nil
}

// ModelCatalogPath is where the admin page keeps its model list, next to
// accounts.json. When it exists it replaces KIRO_MODELS_FILE at startup.
func (p *AccountPool) ModelCatalogPath() string {
	return filepath.Join(filepath.Dir(p.path), "models.json")
}

// SaveModelCatalog writes the catalog atomically, keeping the previous version.
func (p *AccountPool) SaveModelCatalog(data []byte) error {
	path := p.ModelCatalogPath()
	if prev, err := os.ReadFile(path); err == nil {
		if err = atomicPrivateFile(filepath.Join(filepath.Dir(path), "models.previous.json"), prev); err != nil {
			return errors.New("模型列表备份失败，变更未生效")
		}
	}
	if err := atomicPrivateFile(path, data); err != nil {
		return errors.New("模型列表保存失败，变更未生效")
	}
	return nil
}
