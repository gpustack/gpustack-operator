package broker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/utils/json"
)

func TestLedgerResumesOnlyUnissuedInitialization(t *testing.T) {
	const identity = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name     string
		files    []string
		counter  string
		pending  bool
		wantFail bool
	}{
		{name: "before identity", pending: true},
		{name: "before layout", pending: true, files: []string{identityFile}},
		{name: "before counter", pending: true, files: []string{identityFile, "layout"}},
		{name: "before completion", pending: true, files: []string{identityFile, "layout", counterFile}, counter: "0\n"},
		{name: "unrelated files", pending: true, files: []string{"unrelated", "manifest-staging/orphan"}},
		{name: "issued generation", pending: true, files: []string{identityFile, "layout", counterFile}, counter: "1\n", wantFail: true},
		{name: "generation records", pending: true, files: []string{recordsDir + "/g.json"}, wantFail: true},
		{name: "retained sessions", pending: true, files: []string{sessionsFile}, wantFail: true},
		{name: "lost established layout", files: []string{identityFile, counterFile}, counter: "0\n", wantFail: true},
		{name: "lost established identity", files: []string{"layout", counterFile}, counter: "0\n", wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, recordsDir), 0o755))
			raw, err := json.Marshal(ledgerLayout{LayoutVersion: layoutVersion, Scope: testScope, Identity: identity})
			require.NoError(t, err)
			if tc.pending {
				require.NoError(t, writeSynced(filepath.Join(root, initializingFile), raw))
			}
			contents := map[string][]byte{identityFile: []byte(identity + "\n"), "layout": raw, counterFile: []byte(tc.counter)}
			for _, name := range tc.files {
				require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755))
				require.NoError(t, writeSynced(filepath.Join(root, name), contents[name]))
			}
			l, fresh, err := openLedger(root, testScope)
			if tc.wantFail {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.True(t, fresh)
			require.NoFileExists(t, filepath.Join(root, initializingFile))
			if tc.name == "unrelated files" {
				require.FileExists(t, filepath.Join(root, "unrelated"))
				require.FileExists(t, filepath.Join(root, "manifest-staging", "orphan"))
			}
			require.NoError(t, l.ensureCounter(fresh))
			token, err := l.nextGeneration()
			require.NoError(t, err)
			require.Equal(t, "g-"+testScope+"-"+identity+"-1", token)
			reopened, fresh, err := openLedger(root, testScope)
			require.NoError(t, err)
			require.False(t, fresh)
			token, err = reopened.nextGeneration()
			require.NoError(t, err)
			require.Equal(t, "g-"+testScope+"-"+identity+"-2", token)
		})
	}
}
