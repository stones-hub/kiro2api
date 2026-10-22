package server

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
	"kiro2api/config"
)

// Model list management: show the live catalog, compare it with what Kiro
// offers an account right now, and add or remove models without a restart.

const modelCheckTTL = 15 * time.Minute

type upstreamModelDiff struct {
	config.CatalogModelView
	// new: not served yet; changed: served with different limits or controls; same.
	Status string `json:"status"`
}

type modelCheck struct {
	at      time.Time
	raw     map[string]json.RawMessage
	account string
}

func registerModelRoutes(r *gin.Engine, pool *auth.AccountPool, readJSON func(*gin.Context, any) bool, fail func(*gin.Context, error)) {
	var mu sync.Mutex
	var last modelCheck

	r.GET("/api/models", func(c *gin.Context) { c.JSON(200, config.CatalogSnapshot()) })

	r.POST("/api/models/check", func(c *gin.Context) {
		var in struct {
			AccountID string `json:"account_id"`
		}
		if !readJSON(c, &in) {
			return
		}
		up, err := pool.FetchUpstreamModels(c.Request.Context(), in.AccountID)
		if err != nil {
			fail(c, err)
			return
		}
		current := map[string]config.CatalogModelView{}
		snap := config.CatalogSnapshot()
		for _, m := range snap.Models {
			current[m.ID] = m
		}
		diff := []upstreamModelDiff{}
		raw := map[string]json.RawMessage{}
		invalid := []string{}
		for _, r := range up.Models {
			v, err := config.PrepareModelView(r)
			if err != nil {
				// One odd entry must not hide the rest of the list.
				invalid = append(invalid, err.Error())
				continue
			}
			if _, dup := raw[v.ID]; dup {
				continue
			}
			raw[v.ID] = r
			status := "new"
			if cur, ok := current[v.ID]; ok {
				status = "same"
				if !config.SameModel(cur, v) {
					status = "changed"
				}
				delete(current, v.ID)
			}
			diff = append(diff, upstreamModelDiff{CatalogModelView: v, Status: status})
		}
		missing := []string{}
		for _, m := range snap.Models {
			if _, ok := current[m.ID]; ok {
				missing = append(missing, m.ID)
			}
		}
		mu.Lock()
		last = modelCheck{at: time.Now(), raw: raw, account: up.AccountID}
		mu.Unlock()
		c.JSON(200, gin.H{"revision": snap.Revision, "account_id": up.AccountID, "account": up.Account, "plan_type": up.Plan,
			"checked_at": time.Now().UTC(), "models": diff, "missing": missing, "invalid": invalid})
	})

	r.POST("/api/models/apply", func(c *gin.Context) {
		var in struct {
			Revision int      `json:"revision"`
			Add      []string `json:"add"`
			Remove   []string `json:"remove"`
		}
		if !readJSON(c, &in) {
			return
		}
		mu.Lock()
		check := last
		mu.Unlock()
		upsert := []json.RawMessage{}
		if len(in.Add) > 0 {
			if check.raw == nil || time.Since(check.at) > modelCheckTTL {
				fail(c, errors.New("检测结果已过期，请重新检测线上模型"))
				return
			}
			for _, id := range in.Add {
				r, ok := check.raw[id]
				if !ok {
					fail(c, errors.New("模型不在最近一次检测结果中: "+id))
					return
				}
				upsert = append(upsert, r)
			}
		}
		err := config.UpdateCatalog(in.Revision, upsert, in.Remove, pool.SaveModelCatalog)
		if errors.Is(err, config.ErrCatalogConflict) {
			c.JSON(409, gin.H{"error": err.Error()})
			return
		}
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(200, config.CatalogSnapshot())
	})
}
