package credskeeper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// session is what the keeper saves between runs, so the password can be
// removed after the first run. It never holds the credential.
type session struct {
	Token         string `json:"token"`
	CredsRevision string `json:"creds_revision"`
}

// loadSession returns the saved session, or an empty one. A missing or
// unreadable file is not an error: the keeper then signs in with the
// password, as on a first run.
func loadSession(path string, log *slog.Logger) session {
	var s session
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warn("cannot read the session file", "path", path, "error", err)
		}
		return s
	}
	if err := json.Unmarshal(data, &s); err != nil {
		log.Warn("ignoring an unreadable session file", "path", path, "error", err)
		return session{}
	}
	return s
}

// saveSession writes the session file, always 0600: the token gives
// platform access as the Thing.
func saveSession(path string, s session) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := writeAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

// writeIfChanged writes data to path unless the file already holds it, and
// reports whether it wrote.
func writeIfChanged(path string, data []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, os.Chmod(path, mode)
	}
	if err := writeAtomic(path, data, mode); err != nil {
		return false, fmt.Errorf("write creds file: %w", err)
	}
	return true, nil
}

// writeAtomic writes a temporary file in the same directory and renames it
// over path, so no reader ever sees half a file. The directory must exist:
// it is usually a volume or a directory the operator made with the right
// owner, and a directory made here could have the wrong one.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
