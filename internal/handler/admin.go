package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/teddyli18000/mimo-web2api/internal/config"
	"github.com/teddyli18000/mimo-web2api/internal/pool"
	"github.com/teddyli18000/mimo-web2api/internal/stats"
)

type AdminHandler struct {
	pool *pool.Pool
}

func NewAdminHandler(p *pool.Pool) *AdminHandler {
	return &AdminHandler{pool: p}
}

func (h *AdminHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.Get()
	writeJSON(w, cfg)
}

func (h *AdminHandler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	var update struct {
		DefaultModel string `json:"default_model,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	config.Update(func(cfg *config.Config) {
		if update.DefaultModel != "" {
			cfg.DefaultModel = update.DefaultModel
		}
	})
	config.Save()
	writeJSON(w, map[string]string{"status": "ok"})
}

// accountRequest 是管理接口的账号入参。
// Active 用指针区分「省略」与「显式 false」：省略视为启用，
// 避免客户端漏字段时账号写进配置却不进池（旧实现会被静默降级为 Disabled）。
type accountRequest struct {
	ID           string `json:"id"`
	ServiceToken string `json:"service_token"`
	UserID       string `json:"user_id"`
	Ph           string `json:"ph"`
	Active       *bool  `json:"active"`
}

func (h *AdminHandler) AddAccount(w http.ResponseWriter, r *http.Request) {
	var req accountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}

	// 四项都是上游请求的必需品：buildCookie 需要 userId/serviceToken/ph，
	// SaveConversation 还要把 ph 拼进 query。缺一项写进去也只是调不通的空壳。
	// 错误只回字段名，不回显任何凭据值。
	var missing []string
	if strings.TrimSpace(req.ID) == "" {
		missing = append(missing, "id")
	}
	if strings.TrimSpace(req.ServiceToken) == "" {
		missing = append(missing, "service_token")
	}
	if strings.TrimSpace(req.UserID) == "" {
		missing = append(missing, "user_id")
	}
	if strings.TrimSpace(req.Ph) == "" {
		missing = append(missing, "ph")
	}
	if len(missing) > 0 {
		writeError(w, http.StatusBadRequest, "missing required fields: "+strings.Join(missing, ", "))
		return
	}

	acc := config.Account{
		ID:           strings.TrimSpace(req.ID),
		ServiceToken: req.ServiceToken,
		UserID:       req.UserID,
		Ph:           req.Ph,
		Active:       req.Active == nil || *req.Active,
	}

	// 幂等写入：同一账号（同 user_id）重复同步只更新，不追加新行。
	var result config.UpsertResult
	config.Update(func(cfg *config.Config) {
		cfg.Accounts, result = config.UpsertAccount(cfg.Accounts, acc)
	})
	config.Save()
	h.pool.Reload(config.Get().Accounts)
	writeJSON(w, map[string]string{"status": result.Action, "id": result.ID})
}

func (h *AdminHandler) DeleteAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	found := false
	config.Update(func(cfg *config.Config) {
		filtered := make([]config.Account, 0, len(cfg.Accounts))
		for _, acc := range cfg.Accounts {
			if acc.ID == req.ID {
				found = true
				continue
			}
			filtered = append(filtered, acc)
		}
		cfg.Accounts = filtered
	})
	if !found {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	config.Save()
	h.pool.Reload(config.Get().Accounts)
	writeJSON(w, map[string]string{"status": "deleted"})
}

func (h *AdminHandler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.pool.HealthCheck(r.Context()))
}

// GetStats 返回 token 用量统计
func (h *AdminHandler) GetStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, stats.Get().GetStats())
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
