package handler

import (
	"context"
	"log"

	"github.com/teddyli18000/mimo-web2api/internal/adapter"
	"github.com/teddyli18000/mimo-web2api/internal/mimo"
)

// 上游没有 OpenAI 那种 image_url 字段，图片必须先传到它的资源接口、拿到资源 id，
// 再放进对话请求的 multiMedias。此前网关恒发空数组，等于把图片静默丢掉
// （用户发图后模型会回答"我没看到图片"）。

// collectImagesOpenAI 从 OpenAI 消息里取出图片 data URL
func collectImagesOpenAI(msgs []adapter.OpenAIMessage) []string {
	var out []string
	for _, m := range msgs {
		blocks, ok := m.Content.([]interface{})
		if !ok {
			continue
		}
		for _, b := range blocks {
			bm, ok := b.(map[string]interface{})
			if !ok || bm["type"] != "image_url" {
				continue
			}
			switch v := bm["image_url"].(type) {
			case string:
				out = append(out, v)
			case map[string]interface{}:
				if u, ok := v["url"].(string); ok && u != "" {
					out = append(out, u)
				}
			}
		}
	}
	return out
}

// collectImagesAnthropic 从 Anthropic 消息里取出图片，统一转成 data URL
func collectImagesAnthropic(msgs []adapter.AnthropicMessage) []string {
	var out []string
	for _, m := range msgs {
		blocks, ok := m.Content.([]interface{})
		if !ok {
			continue
		}
		for _, b := range blocks {
			bm, ok := b.(map[string]interface{})
			if !ok || bm["type"] != "image" {
				continue
			}
			src, ok := bm["source"].(map[string]interface{})
			if !ok {
				continue
			}
			// Anthropic 用 base64 块；url 形式（部分客户端扩展）也接受
			if data, ok := src["data"].(string); ok && data != "" {
				mime, _ := src["media_type"].(string)
				if mime == "" {
					mime = "image/png"
				}
				out = append(out, "data:"+mime+";base64,"+data)
				continue
			}
			if u, ok := src["url"].(string); ok && u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// uploadImages 把 data URL 图片传到上游，返回 multiMedias 数组。
// 单张失败只跳过它并记日志——不能因为一张图的问题让整个请求失败。
func uploadImages(ctx context.Context, client *mimo.WebClient, dataURLs []string) []interface{} {
	if len(dataURLs) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(dataURLs))
	for _, u := range dataURLs {
		data, mime, ok := mimo.MediaFromDataURL(u)
		if !ok {
			log.Printf("[media] 跳过非 data URL 图片（暂不支持直链，长度=%d）", len(u))
			continue
		}
		obj, err := client.UploadMedia(ctx, data, mime)
		if err != nil {
			log.Printf("[media] 上传失败（%s, %d 字节）: %v", mime, len(data), err)
			continue
		}
		log.Printf("[media] 已上传 %s（%d 字节）→ 资源 id=%v", mime, len(data), obj["url"])
		out = append(out, obj)
	}
	return out
}

// textFile 是待上传的文本附件
type textFile struct {
	Name string
	Data []byte
}

// collectTextFilesOpenAI 取 OpenAI 的 file 内容块：
// {type:"file", file:{filename, file_data:"data:text/plain;base64,..."}}
func collectTextFilesOpenAI(msgs []adapter.OpenAIMessage) []textFile {
	var out []textFile
	for _, m := range msgs {
		blocks, ok := m.Content.([]interface{})
		if !ok {
			continue
		}
		for _, b := range blocks {
			bm, ok := b.(map[string]interface{})
			if !ok || bm["type"] != "file" {
				continue
			}
			f, ok := bm["file"].(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := f["filename"].(string)
			raw, _ := f["file_data"].(string)
			if raw == "" {
				raw, _ = f["data"].(string)
			}
			if data, _, ok := mimo.MediaFromDataURL(raw); ok {
				out = append(out, textFile{Name: name, Data: data})
			}
		}
	}
	return out
}

// collectTextFilesAnthropic 取 Anthropic 的 document 块（text/plain 的 base64）。
func collectTextFilesAnthropic(msgs []adapter.AnthropicMessage) []textFile {
	var out []textFile
	for _, m := range msgs {
		blocks, ok := m.Content.([]interface{})
		if !ok {
			continue
		}
		for _, b := range blocks {
			bm, ok := b.(map[string]interface{})
			if !ok || bm["type"] != "document" {
				continue
			}
			src, ok := bm["source"].(map[string]interface{})
			if !ok || src["type"] != "base64" {
				continue
			}
			data, _ := src["data"].(string)
			mime, _ := src["media_type"].(string)
			if data == "" || (mime != "" && mime != "text/plain") {
				continue
			}
			if raw, _, ok := mimo.MediaFromDataURL("data:text/plain;base64," + data); ok {
				name, _ := bm["title"].(string)
				out = append(out, textFile{Name: name, Data: raw})
			}
		}
	}
	return out
}

// uploadFiles 把文本文件传到上游资源接口，返回 mediaType=file 的 multiMedias 条目。
func uploadFiles(ctx context.Context, client *mimo.WebClient, files []textFile) []interface{} {
	if len(files) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(files))
	for _, f := range files {
		obj, err := client.UploadFile(ctx, f.Data, f.Name)
		if err != nil {
			log.Printf("[media] 文本文件上传失败（%s, %d 字节）: %v", f.Name, len(f.Data), err)
			continue
		}
		log.Printf("[media] 已上传文本文件 %s（%d 字节）→ 资源 id=%v", f.Name, len(f.Data), obj["url"])
		out = append(out, obj)
	}
	return out
}
