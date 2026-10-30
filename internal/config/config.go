package config

import (
	"encoding/json"
	"os"
	"sync"
)

type Config struct {
	Port         string    `json:"port"`
	APIKey       string    `json:"api_key"` // 网关自身的认证 Key
	DefaultModel string    `json:"default_model"`
	Accounts     []Account `json:"accounts"`
}

// Account 网页端账号配置
type Account struct {
	ID           string `json:"id"`
	ServiceToken string `json:"service_token"`
	UserID       string `json:"user_id"`
	Ph           string `json:"ph"`
	Active       bool   `json:"active"`
}

// UpsertResult 描述一次写入的结果，供管理接口区分新增与更新。
type UpsertResult struct {
	Action string // "added" | "updated"
	ID     string // 生效的账号 ID（按 UserID 命中时保留原 ID）
}

// UpsertAccount 幂等写入账号。
//
// 匹配优先级：先按 ID 命中，再按非空 UserID 命中；都未命中才追加。
// 按 UserID 命中时保留已有 ID —— 健康检查的结果 map、按 ID 删除、面板展示都依赖它稳定。
// 命中后顺带收敛同一 UserID 的历史重复行（此前每次同步都追加，配置里可能已堆了多条），
// 只保留被命中的那一条，其余同 UserID 行丢弃。
//
// 返回新切片而非原地改写：Get 是浅拷贝，Accounts 与全局共享底层数组，
// 原地写会让并发读取方看到半更新状态。
func UpsertAccount(list []Account, acc Account) ([]Account, UpsertResult) {
	target := -1
	for i, existing := range list {
		if acc.ID != "" && existing.ID == acc.ID {
			target = i
			break
		}
	}
	if target < 0 && acc.UserID != "" {
		for i, existing := range list {
			if existing.UserID == acc.UserID {
				target = i
				break
			}
		}
	}

	if target < 0 {
		out := make([]Account, 0, len(list)+1)
		out = append(out, list...)
		out = append(out, acc)
		return out, UpsertResult{Action: "added", ID: acc.ID}
	}

	// 命中：沿用已有 ID，用新凭据覆盖，并丢弃其余同 UserID 的重复行。
	merged := acc
	merged.ID = list[target].ID

	out := make([]Account, 0, len(list))
	for i, existing := range list {
		if i == target {
			out = append(out, merged)
			continue
		}
		if acc.UserID != "" && existing.UserID == acc.UserID {
			continue // 历史重复行
		}
		out = append(out, existing)
	}
	return out, UpsertResult{Action: "updated", ID: merged.ID}
}

var (
	cfg  *Config
	mu   sync.RWMutex
	path string
)

func Load(p string) (*Config, error) {
	path = p
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// 首次运行：生成默认配置并落盘，随后仍要走环境变量覆盖
		cfg = &Config{Port: "8080", APIKey: "sk-mimo", DefaultModel: "mimo-v2.6-pro"}
		if err := Save(); err != nil {
			return nil, err
		}
	} else {
		cfg = &Config{}
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}
	// 环境变量覆盖（让 .env.example 名副其实；首次运行同样生效）
	if v := os.Getenv("PORT"); v != "" {
		cfg.Port = v
	}
	if v := os.Getenv("API_KEY"); v != "" {
		cfg.APIKey = v
	}
	return cfg, nil
}

func Get() Config {
	mu.RLock()
	defer mu.RUnlock()
	return *cfg
}

func Save() error {
	mu.RLock()
	data, err := json.MarshalIndent(cfg, "", "  ")
	pathCopy := path
	mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(pathCopy, data, 0644)
}

func Update(fn func(*Config)) {
	mu.Lock()
	fn(cfg)
	mu.Unlock()
}
