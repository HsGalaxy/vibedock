package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vibedock/internal/model"
	"vibedock/internal/store"
)

func newEnvironmentTestServer(t *testing.T) (*store.Store, *fakeRuntime, http.Handler) {
	t.Helper()
	temporary := t.TempDir()
	st, err := store.New(filepath.Join(temporary, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{}
	server := NewServer(st, runtime, []model.Template{{ID: "node", Name: "Node", Image: "vibedock/workspace:test"}}, os.DirFS(temporary), slog.Default(), testKeyring(t))
	return st, runtime, server.Handler()
}

func TestEnvironmentProfileCRUDAndWorkspaceApplication(t *testing.T) {
	st, runtime, handler := newEnvironmentTestServer(t)
	createProfile := httptest.NewRequest(http.MethodPost, "/api/environment-profiles", strings.NewReader(`{
		"name":"Web 工具链",
		"description":"Node 和 Python 常用工具",
		"icon":"W",
		"base_template_id":"node",
		"cpus":4,
		"memory_mb":8192,
		"apt_packages":["ripgrep"],
		"npm_modules":["typescript@5"],
		"python_packages":["httpx==0.28.1"],
		"environment":{"APP_MODE":"development"},
		"init_script":"touch /workspace/.ready"
	}`))
	createProfile.Header.Set("Content-Type", "application/json")
	profileResponse := httptest.NewRecorder()
	handler.ServeHTTP(profileResponse, createProfile)
	if profileResponse.Code != http.StatusCreated {
		t.Fatalf("profile status %d: %s", profileResponse.Code, profileResponse.Body.String())
	}
	var profile model.EnvironmentProfile
	if err := json.Unmarshal(profileResponse.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	if !idPattern.MatchString(profile.ID) || profile.BaseTemplateID != "node" {
		t.Fatalf("unexpected profile: %#v", profile)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{
		"name": "custom-demo", "template_id": "ignored", "environment_profile_id": profile.ID,
		"agent": "shell", "cpus": "4", "memory_mb": "8192", "environment": "APP_MODE=test\nFEATURE_FLAG=true",
	} {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	createWorkspace := httptest.NewRequest(http.MethodPost, "/api/workspaces", &body)
	createWorkspace.Header.Set("Content-Type", writer.FormDataContentType())
	workspaceResponse := httptest.NewRecorder()
	handler.ServeHTTP(workspaceResponse, createWorkspace)
	if workspaceResponse.Code != http.StatusCreated {
		t.Fatalf("workspace status %d: %s", workspaceResponse.Code, workspaceResponse.Body.String())
	}
	if runtime.lastCreate.Image != "vibedock/workspace:test" || runtime.lastCreate.Environment["APP_MODE"] != "test" || runtime.lastCreate.Environment["FEATURE_FLAG"] != "true" {
		t.Fatalf("profile was not merged into create request: %#v", runtime.lastCreate)
	}
	if !runtime.initialized || len(runtime.lastSetup.AptPackages) != 1 || runtime.lastSetup.InitScript == "" {
		t.Fatalf("environment setup was not applied: %#v", runtime.lastSetup)
	}
	workspaces := st.List()
	if len(workspaces) != 1 || workspaces[0].EnvironmentProfileID != profile.ID || workspaces[0].EnvironmentProfileName != "Web 工具链" {
		t.Fatalf("workspace did not retain profile identity: %#v", workspaces)
	}

	deleteProfile := httptest.NewRequest(http.MethodDelete, "/api/environment-profiles/"+profile.ID, nil)
	deleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(deleteResponse, deleteProfile)
	if deleteResponse.Code != http.StatusNoContent || len(st.ListEnvironmentProfiles()) != 0 {
		t.Fatalf("delete status %d: %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	if st.List()[0].EnvironmentProfileName != "Web 工具链" {
		t.Fatal("deleting a profile changed an existing workspace")
	}
}

func TestEnvironmentProfileRejectsSecretsAndPackageCommands(t *testing.T) {
	_, _, handler := newEnvironmentTestServer(t)
	for _, payload := range []string{
		`{"name":"secret","base_template_id":"node","environment":{"API_TOKEN":"secret"}}`,
		`{"name":"command","base_template_id":"node","apt_packages":["git;id"]}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/environment-profiles", strings.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("expected bad request, got %d: %s", response.Code, response.Body.String())
		}
	}
}
