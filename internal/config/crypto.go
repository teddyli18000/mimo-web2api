package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 凭据在磁盘上加密保存：config.json 里只出现 enc:v1:<base64>。
// 密钥放在同目录的 secret.key（32 字节，0600），进程启动时读一次。
// 内存里的 Config 始终是明文，其余代码不需要感知。
const encPrefix = "enc:v1:"

type credentialCipher struct {
	aead cipher.AEAD
}

func loadOrCreateCipher(configPath string) (*credentialCipher, error) {
	keyPath := filepath.Join(filepath.Dir(configPath), "secret.key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		// 0600：Windows 上会被忽略，Linux/macOS 生效
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			return nil, err
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret.key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &credentialCipher{aead: aead}, nil
}

func (c *credentialCipher) encrypt(plain string) (string, error) {
	if plain == "" || strings.HasPrefix(plain, encPrefix) {
		return plain, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *credentialCipher) decrypt(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", err
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// decryptAccounts 就地解密；返回 true 表示文件里还有明文凭据，需要重新加密落盘。
func decryptAccounts(cfg *Config, c *credentialCipher) (bool, error) {
	upgraded := false
	for i := range cfg.Accounts {
		fields := []*string{
			&cfg.Accounts[i].ServiceToken,
			&cfg.Accounts[i].UserID,
			&cfg.Accounts[i].Ph,
		}
		for _, f := range fields {
			if *f == "" {
				continue
			}
			if !strings.HasPrefix(*f, encPrefix) {
				upgraded = true
				continue
			}
			plain, err := c.decrypt(*f)
			if err != nil {
				return false, fmt.Errorf("account %q: %w", cfg.Accounts[i].ID, err)
			}
			*f = plain
		}
	}
	return upgraded, nil
}
