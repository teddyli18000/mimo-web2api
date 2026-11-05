package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialEncryptionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")

	if _, err := Load(p); err != nil {
		t.Fatalf("load: %v", err)
	}
	Update(func(c *Config) {
		c.Accounts = []Account{{ID: "a1", ServiceToken: "tok-secret", UserID: "42", Ph: "ph==", Active: true}}
	})
	if err := Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(raw)
	for _, secret := range []string{"tok-secret", "ph==", `"42"`} {
		if strings.Contains(text, secret) {
			t.Fatalf("config.json still contains plaintext %q:\n%s", secret, text)
		}
	}
	if !strings.Contains(text, encPrefix) {
		t.Fatalf("config.json has no encrypted fields:\n%s", text)
	}

	if _, err := os.Stat(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatalf("secret.key missing: %v", err)
	}

	// 重新加载应拿回明文
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].ServiceToken != "tok-secret" || cfg.Accounts[0].Ph != "ph==" {
		t.Fatalf("decrypted account mismatch: %+v", cfg.Accounts)
	}
	if cfg.Accounts[0].UserID != "42" {
		t.Fatalf("user_id mismatch: %q", cfg.Accounts[0].UserID)
	}
}

func TestPlaintextConfigUpgradesToEncrypted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	legacy := `{"port":"8080","api_key":"sk-mimo","default_model":"mimo-v2.6-pro","accounts":[{"id":"old","service_token":"legacy-token","user_id":"7","ph":"legacy-ph","active":true}]}`
	if err := os.WriteFile(p, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if cfg.Accounts[0].ServiceToken != "legacy-token" {
		t.Fatalf("plaintext not readable: %+v", cfg.Accounts[0])
	}

	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), "legacy-token") {
		t.Fatalf("legacy plaintext was not re-encrypted:\n%s", raw)
	}
	if !strings.Contains(string(raw), encPrefix) {
		t.Fatalf("upgraded config has no ciphertext:\n%s", raw)
	}
}

func TestCipherRejectsShortCiphertext(t *testing.T) {
	c, err := loadOrCreateCipher(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.decrypt(encPrefix + "AAAA"); err == nil {
		t.Fatal("expected error for short ciphertext")
	}
}
