package preflight

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// inImageLibDir is where this image carries the manufacturer preload-library trees the
// device-manager's init container normally stages onto the host at deviceplugin.OperatorLibDir --
// see pack/gpustack-operator/Dockerfile's GPUSTACK_LIB_DIR (default
// "${GPUSTACK_CONF_DIR}/lib" = "/etc/gpustack/lib"). It is a package var, not a const, so tests can
// point it at a scratch directory instead of the real image root.
var inImageLibDir = "/etc/gpustack/lib"

// StageResult is what staging one manufacturer's lib tree onto the host concluded.
//
// A probe that mounts an injection whose source was never staged starts a container that fails
// with an error naming neither the missing file nor why. Failed and Reason turn that into a named
// outcome instead: a caller must drop the affected steps to the emit fallback -- naming what could
// not be written -- rather than build an injection whose mounts point at nothing.
type StageResult struct {
	// Manufacturer is which manufacturer's tree this result is for.
	Manufacturer string
	// Failed is true when the tree could not be written to the host through the mounted host
	// root. A caller must not build an injection over a Failed result.
	Failed bool
	// Reason names what could not be written, set exactly when Failed is true.
	Reason string
}

// StageLib copies this image's manufacturer tree at inImageLibDir/<manufacturer> onto the host,
// through the mounted host root, at hostRoot + deviceplugin.OperatorLibDir + "/<manufacturer>".
//
// This is the gap a standalone probe container leaves: the device-manager's own init container
// normally does this copy before anything is ever mounted from OperatorLibDir, and a preflight run
// has no init container to do it for it. Nothing here is left half-written and silently mounted
// from: a source this image does not carry, or a host that refuses the write, comes back as a
// Failed StageResult naming why, so the caller can drop the affected steps to the emit fallback
// instead of starting a container whose mounts point at nothing.
func StageLib(hostRoot, manufacturer string) StageResult {
	res := StageResult{Manufacturer: manufacturer}

	src := filepath.Join(inImageLibDir, manufacturer)
	if !osx.ExistsDir(src) {
		res.Failed = true
		res.Reason = fmt.Sprintf("this image carries no %q to stage", src)
		return res
	}

	dst := filepath.Join(hostRoot, deviceplugin.OperatorLibDir, manufacturer)
	if err := copyTree(src, dst); err != nil {
		res.Failed = true
		res.Reason = fmt.Sprintf("write %q: %v", dst, err)
	}
	return res
}

// copyTree recursively copies every file under src onto the same relative path under dst,
// preserving each file's permissions -- the preload libraries and executables this stages must
// keep whatever mode the image gave them.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

// copyFile copies one file from src to dst, creating dst's parent directory and carrying src's
// file mode over.
//
// It writes beside dst and renames over it rather than truncating dst in place, which settles two
// things that matter because this tree is never removed and a real allocation mounts it. A copy
// that fails partway -- a full filesystem, a read error, a killed process -- would otherwise leave
// a truncated library there for every allocation afterwards, reported as a staging failure but
// mounted all the same; a rename within one directory is atomic, so a reader sees either the whole
// old file or the whole new one. And the mode is applied to the temporary, where it always takes:
// opening an existing dst with a mode argument silently keeps the mode dst already had, so an
// executable staged over a non-executable one stayed non-executable.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".stage-*")
	if err != nil {
		return err
	}
	// Removes the temporary on every path that did not rename it away, so a failed copy leaves the
	// destination untouched and nothing beside it.
	defer func() {
		_ = out.Close()
		_ = os.Remove(out.Name())
	}()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	if err = out.Chmod(info.Mode()); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	return os.Rename(out.Name(), dst)
}
