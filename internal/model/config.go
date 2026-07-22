package model

type AccountConfigFile struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Editable bool   `json:"editable"`
	Size     int64  `json:"size,omitempty"`
}

type AccountConfigDocument struct {
	Path           string `json:"path"`
	Exists         bool   `json:"exists"`
	Content        string `json:"content"`
	DefaultContent string `json:"default_content,omitempty"`
}

// AccountConfigRuntime exposes only provider-specific allowlisted text files.
// Authentication material is deliberately outside this interface.
type AccountConfigRuntime interface {
	ListAccountConfig(Context, string, AuthProvider) ([]AccountConfigFile, error)
	ReadAccountConfig(Context, string, AuthProvider, string) (AccountConfigDocument, error)
	WriteAccountConfig(Context, string, AuthProvider, string, string) (AccountConfigDocument, error)
	ResetAccountConfig(Context, string, AuthProvider, string) error
}
