package store

import (
	"path/filepath"
	"testing"
	"time"

	"vibedock/internal/model"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	ws := model.Workspace{
		ID: "abcdef123456", Name: "demo", CreatedAt: time.Now().UTC(),
		Sessions: []model.Session{{ID: "123456abcdef", Name: "Codex", Agent: "codex", AccountID: "aabbccddeeff", Port: 7700}},
	}
	account := model.AccountProfile{
		ID: "aabbccddeeff", Name: "OpenAI API", Agent: "codex", AuthType: "api_key",
		SecretCiphertext: "v1:ciphertext", ExtraEnv: map[string]string{"HTTP_PROXY": "http://proxy"}, CreatedAt: time.Now().UTC(),
	}
	environment := model.EnvironmentProfile{
		ID: "env123456789", Name: "Web tools", BaseTemplateID: "node", CPUs: 4, MemoryMB: 8192,
		AptPackages: []string{"ripgrep"}, Environment: map[string]string{"APP_MODE": "dev"}, CreatedAt: time.Now().UTC(),
	}
	if err := st.Put(ws); err != nil {
		t.Fatal(err)
	}
	if err := st.PutAccount(account); err != nil {
		t.Fatal(err)
	}
	if err := st.PutEnvironmentProfile(environment); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSessionSecret("123456abcdef", "v1:direct-ciphertext"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get(ws.ID)
	if !ok || got.Name != "demo" || len(got.Sessions) != 1 || got.Sessions[0].Port != 7700 || got.Sessions[0].AccountID != account.ID {
		t.Fatalf("unexpected workspace: %#v, ok=%v", got, ok)
	}
	gotAccount, ok := reloaded.GetAccount(account.ID)
	if !ok || gotAccount.SecretCiphertext != "v1:ciphertext" || gotAccount.ExtraEnv["HTTP_PROXY"] == "" {
		t.Fatalf("unexpected account: %#v, ok=%v", gotAccount, ok)
	}
	gotEnvironment, ok := reloaded.GetEnvironmentProfile(environment.ID)
	if !ok || gotEnvironment.Environment["APP_MODE"] != "dev" || len(gotEnvironment.AptPackages) != 1 {
		t.Fatalf("unexpected environment: %#v, ok=%v", gotEnvironment, ok)
	}
	if gotSecret, ok := reloaded.GetSessionSecret("123456abcdef"); !ok || gotSecret != "v1:direct-ciphertext" {
		t.Fatalf("unexpected session secret: %q, ok=%v", gotSecret, ok)
	}
	got.Sessions[0].Name = "mutated"
	gotAccount.ExtraEnv["HTTP_PROXY"] = "mutated"
	gotEnvironment.Environment["APP_MODE"] = "mutated"
	again, _ := reloaded.Get(ws.ID)
	againAccount, _ := reloaded.GetAccount(account.ID)
	againEnvironment, _ := reloaded.GetEnvironmentProfile(environment.ID)
	if again.Sessions[0].Name == "mutated" || againAccount.ExtraEnv["HTTP_PROXY"] == "mutated" || againEnvironment.Environment["APP_MODE"] == "mutated" {
		t.Fatal("store returned a mutable alias")
	}
	if err := reloaded.Delete(ws.ID); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.DeleteAccount(account.ID); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.DeleteEnvironmentProfile(environment.ID); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.DeleteSessionSecret("123456abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get(ws.ID); ok {
		t.Fatal("workspace should be deleted")
	}
	if _, ok := reloaded.GetAccount(account.ID); ok {
		t.Fatal("account should be deleted")
	}
	if _, ok := reloaded.GetEnvironmentProfile(environment.ID); ok {
		t.Fatal("environment should be deleted")
	}
	if _, ok := reloaded.GetSessionSecret("123456abcdef"); ok {
		t.Fatal("session secret should be deleted")
	}
}
