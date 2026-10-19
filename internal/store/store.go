package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"vibedock/internal/model"
)

type Store struct {
	mu             sync.RWMutex
	path           string
	workspaces     map[string]model.Workspace
	accounts       map[string]model.AccountProfile
	environments   map[string]model.EnvironmentProfile
	sessionSecrets map[string]string
}

type diskState struct {
	Workspaces     []model.Workspace          `json:"workspaces"`
	Accounts       []model.AccountProfile     `json:"accounts,omitempty"`
	Environments   []model.EnvironmentProfile `json:"environment_profiles,omitempty"`
	SessionSecrets map[string]string          `json:"session_secrets,omitempty"`
}

func New(path string) (*Store, error) {
	s := &Store{
		path:           path,
		workspaces:     make(map[string]model.Workspace),
		accounts:       make(map[string]model.AccountProfile),
		environments:   make(map[string]model.EnvironmentProfile),
		sessionSecrets: make(map[string]string),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	var state diskState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode state: %w", err)
	}
	for _, ws := range state.Workspaces {
		s.workspaces[ws.ID] = ws
	}
	for _, account := range state.Accounts {
		s.accounts[account.ID] = account
	}
	for _, environment := range state.Environments {
		s.environments[environment.ID] = cloneEnvironment(environment)
	}
	for sessionID, ciphertext := range state.SessionSecrets {
		s.sessionSecrets[sessionID] = ciphertext
	}
	return nil
}

func (s *Store) List() []model.Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]model.Workspace, 0, len(s.workspaces))
	for _, ws := range s.workspaces {
		out = append(out, cloneWorkspace(ws))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (s *Store) Get(id string) (model.Workspace, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ws, ok := s.workspaces[id]
	return cloneWorkspace(ws), ok
}

func (s *Store) Put(ws model.Workspace) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.workspaces[ws.ID]
	s.workspaces[ws.ID] = cloneWorkspace(ws)
	if err := s.saveLocked(); err != nil {
		if existed {
			s.workspaces[ws.ID] = previous
		} else {
			delete(s.workspaces, ws.ID)
		}
		return err
	}
	return nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.workspaces[id]
	delete(s.workspaces, id)
	if err := s.saveLocked(); err != nil {
		if existed {
			s.workspaces[id] = previous
		}
		return err
	}
	return nil
}

func (s *Store) ListAccounts() []model.AccountProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]model.AccountProfile, 0, len(s.accounts))
	for _, account := range s.accounts {
		out = append(out, cloneAccount(account))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (s *Store) GetAccount(id string) (model.AccountProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	account, ok := s.accounts[id]
	return cloneAccount(account), ok
}

func (s *Store) PutAccount(account model.AccountProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.accounts[account.ID]
	s.accounts[account.ID] = cloneAccount(account)
	if err := s.saveLocked(); err != nil {
		if existed {
			s.accounts[account.ID] = previous
		} else {
			delete(s.accounts, account.ID)
		}
		return err
	}
	return nil
}

func (s *Store) DeleteAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.accounts[id]
	delete(s.accounts, id)
	if err := s.saveLocked(); err != nil {
		if existed {
			s.accounts[id] = previous
		}
		return err
	}
	return nil
}

func (s *Store) ListEnvironmentProfiles() []model.EnvironmentProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.EnvironmentProfile, 0, len(s.environments))
	for _, environment := range s.environments {
		out = append(out, cloneEnvironment(environment))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (s *Store) GetEnvironmentProfile(id string) (model.EnvironmentProfile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	environment, ok := s.environments[id]
	return cloneEnvironment(environment), ok
}

func (s *Store) PutEnvironmentProfile(environment model.EnvironmentProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.environments[environment.ID]
	s.environments[environment.ID] = cloneEnvironment(environment)
	if err := s.saveLocked(); err != nil {
		if existed {
			s.environments[environment.ID] = previous
		} else {
			delete(s.environments, environment.ID)
		}
		return err
	}
	return nil
}

func (s *Store) DeleteEnvironmentProfile(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.environments[id]
	delete(s.environments, id)
	if err := s.saveLocked(); err != nil {
		if existed {
			s.environments[id] = previous
		}
		return err
	}
	return nil
}

func (s *Store) GetSessionSecret(sessionID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.sessionSecrets[sessionID]
	return value, ok
}

func (s *Store) PutSessionSecret(sessionID, ciphertext string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.sessionSecrets[sessionID]
	if ciphertext == "" {
		delete(s.sessionSecrets, sessionID)
	} else {
		s.sessionSecrets[sessionID] = ciphertext
	}
	if err := s.saveLocked(); err != nil {
		if existed {
			s.sessionSecrets[sessionID] = previous
		} else {
			delete(s.sessionSecrets, sessionID)
		}
		return err
	}
	return nil
}

func (s *Store) DeleteSessionSecret(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.sessionSecrets[sessionID]
	delete(s.sessionSecrets, sessionID)
	if err := s.saveLocked(); err != nil {
		if existed {
			s.sessionSecrets[sessionID] = previous
		}
		return err
	}
	return nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	state := diskState{
		Workspaces:     make([]model.Workspace, 0, len(s.workspaces)),
		Accounts:       make([]model.AccountProfile, 0, len(s.accounts)),
		Environments:   make([]model.EnvironmentProfile, 0, len(s.environments)),
		SessionSecrets: make(map[string]string, len(s.sessionSecrets)),
	}
	for _, ws := range s.workspaces {
		state.Workspaces = append(state.Workspaces, cloneWorkspace(ws))
	}
	for _, account := range s.accounts {
		state.Accounts = append(state.Accounts, cloneAccount(account))
	}
	for _, environment := range s.environments {
		state.Environments = append(state.Environments, cloneEnvironment(environment))
	}
	for sessionID, ciphertext := range s.sessionSecrets {
		state.SessionSecrets[sessionID] = ciphertext
	}
	sort.Slice(state.Workspaces, func(i, j int) bool {
		return state.Workspaces[i].CreatedAt.Before(state.Workspaces[j].CreatedAt)
	})
	sort.Slice(state.Accounts, func(i, j int) bool {
		return state.Accounts[i].CreatedAt.Before(state.Accounts[j].CreatedAt)
	})
	sort.Slice(state.Environments, func(i, j int) bool {
		return state.Environments[i].CreatedAt.Before(state.Environments[j].CreatedAt)
	})
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".vibedock-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func cloneWorkspace(ws model.Workspace) model.Workspace {
	ws.EnvKeys = append([]string(nil), ws.EnvKeys...)
	ws.Sessions = append([]model.Session(nil), ws.Sessions...)
	for i := range ws.Sessions {
		if ws.Sessions[i].DirectConfig != nil {
			copyConfig := *ws.Sessions[i].DirectConfig
			ws.Sessions[i].DirectConfig = &copyConfig
		}
	}
	return ws
}

func cloneEnvironment(environment model.EnvironmentProfile) model.EnvironmentProfile {
	environment.AptPackages = append([]string(nil), environment.AptPackages...)
	environment.NPMModules = append([]string(nil), environment.NPMModules...)
	environment.PythonPackages = append([]string(nil), environment.PythonPackages...)
	if environment.Environment != nil {
		cloned := make(map[string]string, len(environment.Environment))
		for key, value := range environment.Environment {
			cloned[key] = value
		}
		environment.Environment = cloned
	}
	return environment
}

func cloneAccount(account model.AccountProfile) model.AccountProfile {
	if account.ExtraEnv != nil {
		original := account.ExtraEnv
		account.ExtraEnv = make(map[string]string, len(original))
		for key, value := range original {
			account.ExtraEnv[key] = value
		}
	}
	return account
}
