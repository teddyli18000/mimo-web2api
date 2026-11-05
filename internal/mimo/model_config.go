package mimo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// modelConfigAPI 是上游公开的模型配置端点：不需要 Cookie，返回
// data.modelConfigList，每条带 model / name / 简介 / 是否支持图片与文件。
const modelConfigAPI = "/open-apis/bot/config"

// ModelInfo 是上游模型配置的一条记录（只保留网关用得到的字段）。
// 注意别和请求体里的 ModelConfig（modelConfig 字段）重名。
type ModelInfo struct {
	Model             string `json:"model"`
	Name              string `json:"name"`
	Intro             string `json:"cnIntro"`
	ShowImage         bool   `json:"showImage"`
	ShowFile          bool   `json:"showFile"`
	ThinkingDefaultOn bool   `json:"thinkingDefaultOn"`
}

// FetchModels 拉取上游模型列表。调用方负责缓存；失败时返回错误由调用方回退。
func FetchModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webBaseURL+modelConfigAPI, nil)
	if err != nil {
		return nil, err
	}
	setBrowserHeaders(req.Header)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("model config: status %d: %s", resp.StatusCode, string(raw))
	}

	var payload struct {
		Code int `json:"code"`
		Data struct {
			List   []ModelInfo `json:"modelConfigList"`
			ListNg []ModelInfo `json:"modelConfigListNg"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Code != 0 {
		return nil, fmt.Errorf("model config: code %d", payload.Code)
	}

	list := payload.Data.List
	if len(list) == 0 {
		list = payload.Data.ListNg
	}
	out := make([]ModelInfo, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, m := range list {
		if m.Model == "" || seen[m.Model] {
			continue
		}
		seen[m.Model] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("model config: empty list")
	}
	return out, nil
}
