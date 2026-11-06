package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teddyli18000/mimo-web2api/internal/mimo"
	"github.com/teddyli18000/mimo-web2api/internal/router"
)

// 模型列表来自上游公开的 /open-apis/bot/config，缓存 10 分钟。
// 拉取失败时回退到内置列表 —— 模型列表挂掉不该让整个 /v1/models 500。
const modelCacheTTL = 10 * time.Minute

var modelCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	models    []mimo.ModelInfo
}

func fetchModelsCached(force bool) []mimo.ModelInfo {
	modelCache.mu.Lock()
	defer modelCache.mu.Unlock()

	if !force && len(modelCache.models) > 0 && time.Since(modelCache.fetchedAt) < modelCacheTTL {
		return modelCache.models
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	models, err := mimo.FetchModels(ctx)
	if err != nil || len(models) == 0 {
		if err != nil {
			log.Printf("[models] fetch from upstream failed: %v", err)
		} else {
			log.Printf("[models] upstream returned an empty model list")
		}
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

// ModelsHandler 是 /v1/models 与 /v1/models/{id}：优先上游实时列表，失败回退内置。
//
// 带 {id} 时按 OpenAI 规范返回单个模型对象。旧实现无视路径参数、永远回列表，
// 于是 GET /v1/models/xxx 拿到的是 {"object":"list","data":[…]}，
// 按对象取 id 的客户端会直接拿到 undefined。
func ModelsHandler(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(modelListPayload())
		return
	}

	data, _ := modelListPayload()["data"].([]map[string]interface{})
	for _, m := range data {
		if m["id"] == id {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(m)
			return
		}
	}
	writeError(w, http.StatusNotFound, "model not found: "+id)
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
