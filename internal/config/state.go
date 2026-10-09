package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// State is the state file: the kind detected for each service (docs/architecture.md, service
// kinds, Persistence). It is kinds.json next to host.json. It holds no secrets.
type State struct {
	Kinds map[string]string `json:"kinds"` // detected kind per service name
}

// LoadState reads dir/kinds.json. A missing file is an empty State, not an error. Content that is
// not JSON returns HB-CONFIG-INVALID, without the content in the error.
func LoadState(dir string) (State, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "kinds.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, invalid("kinds.json is not valid JSON")
	}
	return s, nil
}

// SaveState writes s to dir/kinds.json.
func SaveState(dir string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(dir, "kinds.json", append(b, '\n'))
}
