package openaioauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAuthTB(b *testing.B, tokens map[string]any) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "auth.json")
	body, _ := json.Marshal(map[string]any{"auth_mode": "chatgpt", "tokens": tokens})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		b.Fatal(err)
	}
	return path
}

// BenchmarkSourceLoad is the credential read Transport.RoundTrip does on every provider request.
func BenchmarkSourceLoad(b *testing.B) {
	access := jwt(map[string]any{"exp": testNow.Add(time.Hour).Unix()})
	id := jwt(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1"}})
	path := writeAuthTB(b, map[string]any{"access_token": access, "id_token": id, "account_id": "acct-1", "refresh_token": "r"})
	src := source(path)
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := src.Load(); err != nil {
			b.Fatal(err)
		}
	}
}
