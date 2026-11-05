package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/teddyli18000/mimo-web2api/internal/mimo"
	"github.com/teddyli18000/mimo-web2api/internal/router"
)

// 模型列表来自上游公开的 /open-apis/bot/config，缓存 10 分钟。
// 拉取失败时回退到内置列表 —— 模型列表挂掉不该让整个 /v1/models 500。
const modelCacheTTL = 10 * time.Minute

var modelCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	models    []mimo.ModelConfig
}

func fetchModelsCached(force bool) []mimo.ModelConfig {
	modelCache.mu.Lock()
	defer modelCache.mu.Unlock()

	if !force && len(modelCache.models) > 0 && time.Since(modelCache.fetchedAt) < modelCacheTTL {
		return modelCache.models
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	models, err := mimo.FetchModels(ctx)
	if err != nil || len(models) == 0 {
		// 拉不到就继续用旧缓存；连旧缓存都没有时用内置列表
		if len(modelCache.models) > 0 {
			return modelCache.models
		}
		return nil
	}
	modelCache.models = models
	modelCache.fetchedAt = time.Now()
	return models
}

func modelListPayload() map[string]interface{} {
	models := fetchModelsCached(false)
	if len(models) == 0 {
		return map[string]interface{}{"object": "list", "data": router.SupportedModels()}
	}
	now := time.Now().Unix()
	data := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]interface{}{
			"id":          m.Model,
			"object":      "model",
			"created":     now,
			"owned_by":    "xiaomi",
			"name":        m.Name,
			"description": m.Intro,
		})
	}
	return map[string]interface{}{"object": "list", "data": data}
}

// ModelsHandler 是 /v1/models：优先上游实时列表，失败回退内置。
func ModelsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(modelListPayload())
}

// RefreshModelsHandler 强制刷新缓存（管理接口，需 API Key）。
func RefreshModelsHandler(w http.ResponseWriter, r *http.Request) {
	models := fetchModelsCached(true)
	if len(models) == 0 {
		writeError(w, http.StatusBadGateway, "failed to fetch models from upstream")
		return
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "count": len(models)})
}
