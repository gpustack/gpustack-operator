package broker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gpustack.ai/gpustack/pkg/utils/json"
)

const initializingFile = "initializing.json"

// initializeLedger finishes only a recorded, pre-admission initialization.
// An established ledger never recreates missing identity, layout or counters.
func initializeLedger(root, scope string) (bool, error) {
	path := filepath.Join(root, initializingFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(filepath.Join(root, identityFile)); err == nil {
			return false, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if err := validateInitializingRoot(root, false); err != nil {
			return false, err
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return false, err
		}
		raw, err = json.Marshal(ledgerLayout{LayoutVersion: layoutVersion, Scope: scope, Identity: hex.EncodeToString(id[:])})
		if err != nil {
			return false, err
		}
		if err := writeSynced(path, raw); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	var pending ledgerLayout
	if err := json.Unmarshal(raw, &pending); err != nil {
		return false, fmt.Errorf("read ledger initialization: %w", err)
	}
	if pending.LayoutVersion != layoutVersion || pending.Scope != scope || len(pending.Identity) != 32 || !hexPattern.MatchString(pending.Identity) {
		return false, fmt.Errorf("the ledger initialization identity or scope is invalid")
	}
	if err := validateInitializingRoot(root, true); err != nil {
		return false, err
	}
	for _, item := range []struct {
		name string
		raw  []byte
	}{
		{identityFile, []byte(pending.Identity + "\n")},
		{"layout", raw},
		{counterFile, []byte("0\n")},
	} {
		current, err := os.ReadFile(filepath.Join(root, item.name))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := writeSynced(filepath.Join(root, item.name), item.raw); err != nil {
				return false, err
			}
		case err != nil:
			return false, err
		case string(current) != string(item.raw):
			return false, fmt.Errorf("ledger initialization disagrees with %s", item.name)
		}
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	dir, err := os.Open(root)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	return true, dir.Sync()
}

func validateInitializingRoot(root string, pending bool) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch entry.Name() {
		case recordsDir:
			records, err := os.ReadDir(filepath.Join(root, recordsDir))
			if err != nil {
				return err
			}
			if len(records) != 0 {
				return fmt.Errorf("ledger initialization found generation records")
			}
		case "writer.lock", initializingFile, initializingFile + ".tmp":
		case identityFile, "layout", counterFile, identityFile + ".tmp", "layout.tmp", counterFile + ".tmp":
			if !pending {
				return fmt.Errorf("ledger identity is missing from an established root")
			}
		case sessionsFile, sessionsFile + ".tmp", operationDir, operationLayoutFile,
			operationLayoutFile + ".tmp", authorityInventoryFile, authorityInventoryFile + ".tmp", manifestsDir:
			return fmt.Errorf("ledger initialization found unexpected state %q", entry.Name())
		}
	}
	return nil
}
