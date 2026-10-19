package store

import (
	"path/filepath"
	"testing"
	"vibedock/internal/model"
)

func TestWriteFailureRollsBackInMemoryState(t *testing.T) {
	base := t.TempDir()
	st, err := New(filepath.Join(base, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	original := model.Workspace{ID: "abcdef123456", Name: "original"}
	if err := st.Put(original); err != nil {
		t.Fatal(err)
	}
	st.path = filepath.Join(base, "missing", "state.json")
	// A directory where state.json should be a file forces rename to fail.
	st.path = base
	modified := original
	modified.Name = "changed"
	if err := st.Put(modified); err == nil {
		t.Fatal("expected save to fail")
	}
	got, ok := st.Get(original.ID)
	if !ok || got.Name != "original" {
		t.Fatalf("rollback failed: %#v", got)
	}
	if err := st.Delete(original.ID); err == nil {
		t.Fatal("expected delete failure")
	}
	if _, ok := st.Get(original.ID); !ok {
		t.Fatal("delete was not rolled back")
	}
	if err := st.PutAccount(model.AccountProfile{ID: "aabbccddeeff"}); err == nil {
		t.Fatal("expected account save failure")
	}
	if _, ok := st.GetAccount("aabbccddeeff"); ok {
		t.Fatal("account save was not rolled back")
	}
	if err := st.PutSessionSecret("123456abcdef", "secret"); err == nil {
		t.Fatal("expected secret save failure")
	}
	if _, ok := st.GetSessionSecret("123456abcdef"); ok {
		t.Fatal("secret save was not rolled back")
	}
}
