package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"kiro2api/utils"
)

// Client API keys. Only a SHA-256 hash of each key is stored; the key itself is
// returned once, when it is created. KIRO_CLIENT_TOKEN keeps working as the
// built-in key with ID BuiltinKeyID and can only be changed in the server's .env.
const (
	BuiltinKeyID   = "env"
	maxAPIKeys     = 50
	apiKeyPrefix   = "sk-kiro-"
	deletedKeyKeep = 31 * 24 * time.Hour
)

type managedAPIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Hash      string     `json:"hash,omitempty"` // hex SHA-256 of the key; cleared on delete
	Hint      string     `json:"hint"`           // leading characters shown in the UI
	Disabled  bool       `json:"disabled,omitempty"`
	Version   int        `json:"version"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

type APIKeyUsage struct {
	Requests     int64   `json:"requests"`
	Succeeded    int64   `json:"succeeded"`
	Failed       int64   `json:"failed"`
	Rejected     int64   `json:"rejected"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	Credits      float64 `json:"credits"`
}

type APIKeyView struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Hint      string      `json:"hint"`
	Builtin   bool        `json:"builtin"`
	Enabled   bool        `json:"enabled"`
	Version   int         `json:"version"`
	CreatedAt *time.Time  `json:"created_at,omitempty"`
	DeletedAt *time.Time  `json:"deleted_at,omitempty"`
	LastUsed  *time.Time  `json:"last_used,omitempty"`
	Today     APIKeyUsage `json:"today"`
	Last24h   APIKeyUsage `json:"last_24h"`
}

type APIKeyPatch struct {
	Version int     `json:"version"`
	Name    *string `json:"name,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

var ErrKeyMissing = errors.New("API Key 不存在")
var ErrKeyConflict = errors.New("API Key 已被更新，请刷新页面后再操作")
var errBuiltinKey = errors.New("默认 Key 来自服务器 .env 的 KIRO_CLIENT_TOKEN，只能在服务器上修改")

type apiKeyContext struct{}

// WithAPIKeyID tags a request context with the client key that authenticated it.
func WithAPIKeyID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, apiKeyContext{}, id)
}

func apiKeyIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(apiKeyContext{}).(string)
	return id
}

func normalizeKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("请填写 Key 名称")
	}
	if len(name) > 80 || strings.ContainsAny(name, "\r\n\t\x00") {
		return "", errors.New("Key 名称最多 80 字节且不能包含换行")
	}
	return name, nil
}

func validateKeys(d *managedDocument, now time.Time) error {
	ids := map[string]bool{BuiltinKeyID: true}
	kept := d.APIKeys[:0]
	for _, k := range d.APIKeys {
		if k.ID == "" || ids[k.ID] {
			return errors.New("API Key ID 缺失或重复")
		}
		ids[k.ID] = true
		if k.DeletedAt != nil {
			// Usage buckets expire after 31 days, after which the record has nothing to show.
			if now.Sub(*k.DeletedAt) > deletedKeyKeep {
				continue
			}
			k.Hash = ""
		} else if b, err := hex.DecodeString(k.Hash); err != nil || len(b) != sha256.Size {
			return fmt.Errorf("API Key 配置无效: %s", k.ID)
		}
		if _, err := normalizeKeyName(k.Name); err != nil {
			return fmt.Errorf("API Key 配置无效: %s", k.ID)
		}
		kept = append(kept, k)
	}
	d.APIKeys = kept
	return nil
}

// syncKeysLocked publishes the lookup table used on every /v1 request, so
// authentication never waits on the pool mutex.
func (p *AccountPool) syncKeysLocked() {
	index := make(map[[sha256.Size]byte]string, len(p.doc.APIKeys)+1)
	if p.builtinSet {
		index[p.builtinHash] = BuiltinKeyID
	}
	for _, k := range p.doc.APIKeys {
		if k.DeletedAt != nil || k.Disabled {
			continue
		}
		var h [sha256.Size]byte
		if b, err := hex.DecodeString(k.Hash); err == nil && len(b) == sha256.Size {
			copy(h[:], b)
			index[h] = k.ID
		}
	}
	p.keyIndex.Store(&index)
}

// SetBuiltinKey registers KIRO_CLIENT_TOKEN as the always-present default key.
func (p *AccountPool) SetBuiltinKey(raw string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.builtinHash = sha256.Sum256([]byte(raw))
	p.builtinHint = raw[:min(4, len(raw))]
	p.builtinSet = raw != ""
	p.syncKeysLocked()
}

// AuthenticateKey returns the ID of an enabled key. Lookup is by hash, so the
// comparison does not leak the stored key through timing.
func (p *AccountPool) AuthenticateKey(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	index := p.keyIndex.Load()
	if index == nil {
		return "", false
	}
	id, ok := (*index)[sha256.Sum256([]byte(raw))]
	return id, ok
}

func (p *AccountPool) keyIndexLocked(id string) int {
	for i := range p.doc.APIKeys {
		if p.doc.APIKeys[i].ID == id {
			return i
		}
	}
	return -1
}

// knownKeyLocked includes deleted keys, whose usage stays viewable until it expires.
func (p *AccountPool) knownKeyLocked(id string) bool {
	return (id == BuiltinKeyID && p.builtinSet) || p.keyIndexLocked(id) >= 0
}

func (p *AccountPool) touchKeyLocked(id string, now time.Time) {
	if id == BuiltinKeyID {
		p.builtinLastUsed = &now
		return
	}
	if i := p.keyIndexLocked(id); i >= 0 {
		p.doc.APIKeys[i].LastUsed = &now
	}
}

// CreateAPIKey returns the new key's ID and the key itself, which is not stored.
func (p *AccountPool) CreateAPIKey(name string) (string, string, error) {
	name, err := normalizeKeyName(name)
	if err != nil {
		return "", "", err
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	raw := apiKeyPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	sum := sha256.Sum256([]byte(raw))
	k := managedAPIKey{ID: utils.GenerateUUID(), Name: name, Hash: hex.EncodeToString(sum[:]), Hint: raw[:len(apiKeyPrefix)+4], Version: 1, CreatedAt: time.Now().UTC()}
	p.mu.Lock()
	defer p.mu.Unlock()
	live := 0
	for _, o := range p.doc.APIKeys {
		if o.DeletedAt == nil {
			live++
		}
	}
	if live >= maxAPIKeys {
		return "", "", fmt.Errorf("最多创建 %d 个 API Key", maxAPIKeys)
	}
	next := cloneDocument(p.doc)
	next.APIKeys = append(next.APIKeys, k)
	if err = p.commitLocked(next); err != nil {
		return "", "", err
	}
	return k.ID, raw, nil
}

func (p *AccountPool) UpdateAPIKey(id string, patch APIKeyPatch) error {
	if id == BuiltinKeyID {
		return errBuiltinKey
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.keyIndexLocked(id)
	if i < 0 || p.doc.APIKeys[i].DeletedAt != nil {
		return ErrKeyMissing
	}
	if patch.Version != p.doc.APIKeys[i].Version {
		return ErrKeyConflict
	}
	next := cloneDocument(p.doc)
	k := &next.APIKeys[i]
	if patch.Name != nil {
		name, err := normalizeKeyName(*patch.Name)
		if err != nil {
			return err
		}
		k.Name = name
	}
	if patch.Enabled != nil {
		k.Disabled = !*patch.Enabled
	}
	k.Version++
	return p.commitLocked(next)
}

// DeleteAPIKey revokes a key at once; requests already running finish normally.
func (p *AccountPool) DeleteAPIKey(id string, version int) error {
	if id == BuiltinKeyID {
		return errBuiltinKey
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.keyIndexLocked(id)
	if i < 0 || p.doc.APIKeys[i].DeletedAt != nil {
		return ErrKeyMissing
	}
	if version != p.doc.APIKeys[i].Version {
		return ErrKeyConflict
	}
	next := cloneDocument(p.doc)
	now := time.Now().UTC()
	next.APIKeys[i].DeletedAt = &now
	next.APIKeys[i].Hash = ""
	next.APIKeys[i].Version++
	return p.commitLocked(next)
}

func (u *APIKeyUsage) add(b *metricBucket) {
	if b == nil {
		return
	}
	u.Requests += b.Requests
	u.Succeeded += b.Succeeded
	u.Failed += b.Failed
	u.Rejected += b.Rejected
	u.InputTokens += b.InputTokens
	u.OutputTokens += b.OutputTokens
	u.Credits += b.MeteredCredits
}

// APIKeys lists the built-in key first, then created keys including deleted ones.
func (p *AccountPool) APIKeys() []APIKeyView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	usage := func(v *APIKeyView) {
		m := p.doc.Metrics
		if m == nil || m.Keys[v.ID] == nil {
			return
		}
		km := m.Keys[v.ID]
		v.Today.add(km.Days[dayKey(now)])
		for i := 0; i < 24; i++ {
			v.Last24h.add(km.Hours[hourKey(now.Add(-time.Duration(i)*time.Hour))])
		}
	}
	out := []APIKeyView{}
	if p.builtinSet {
		v := APIKeyView{ID: BuiltinKeyID, Name: "默认 Key（环境变量）", Hint: p.builtinHint, Builtin: true, Enabled: true, LastUsed: p.builtinLastUsed}
		usage(&v)
		out = append(out, v)
	}
	for _, k := range p.doc.APIKeys {
		created := k.CreatedAt
		v := APIKeyView{ID: k.ID, Name: k.Name, Hint: k.Hint, Enabled: !k.Disabled && k.DeletedAt == nil, Version: k.Version, CreatedAt: &created, DeletedAt: k.DeletedAt, LastUsed: k.LastUsed}
		usage(&v)
		out = append(out, v)
	}
	return out
}
