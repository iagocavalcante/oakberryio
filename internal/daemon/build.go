package daemon

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// flushWriter adapts an http.ResponseWriter (plus its optional Flusher) to
// an io.Writer that flushes after every write, so output handed to it (e.g.
// a docker build/push's combined stdout+stderr) reaches the client as it's
// produced rather than buffering until the handler returns. Comparable with
// ==, so the same value can be assigned to both an exec.Cmd's Stdout and
// Stderr -- os/exec then serializes writes between the two instead of
// racing them (see exec.Cmd's doc comment on Stdout/Stderr).
type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.flusher != nil {
		f.flusher.Flush()
	}
	return n, err
}

// parseBuildArgsHeader decodes the X-Oak-Build-Args header handleBuild
// reads: a JSON object of build-arg key/value pairs, or an absent/empty
// header for no build args.
func parseBuildArgsHeader(h string) (map[string]string, error) {
	if h == "" {
		return nil, nil
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(h), &args); err != nil {
		return nil, fmt.Errorf("decode X-Oak-Build-Args: %w", err)
	}
	return args, nil
}

// buildDockerBuildArgs constructs the argv for the `docker build` handleBuild
// runs: pinned to linux/amd64 (the box and guest arch -- see cmd/oak's
// runDeploy for why that pin matters), sorted --build-arg flags for
// deterministic argv and output, dockerfilePath and image already resolved
// to their final values. A pure function so its shape is testable without
// actually invoking docker (there's no docker in CI).
func buildDockerBuildArgs(image, dockerfilePath, contextDir string, buildArgs map[string]string) []string {
	keys := make([]string, 0, len(buildArgs))
	for k := range buildArgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := []string{"build", "--platform", "linux/amd64", "-f", dockerfilePath}
	for _, k := range keys {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", k, buildArgs[k]))
	}
	args = append(args, "-t", image, contextDir)
	return args
}

// extractBuildContext extracts a tar stream (as produced by cmd/oak's
// tarContext) into dir, which must already exist. Every entry name is
// validated by safeExtractPath before it touches the filesystem: this is
// the trust boundary between an authenticated but not necessarily
// well-behaved client and the filesystem oakd runs docker build against as
// root, so a malformed or malicious tar must not be able to write outside
// dir.
func extractBuildContext(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		target, err := safeExtractPath(dir, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0777)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", target, err)
			}
		case tar.TypeSymlink:
			// hdr.Linkname is the symlink's target, not a path within the
			// tar -- it isn't run through safeExtractPath and can point
			// anywhere, including outside dir. That matches what `tar`
			// (and docker build's own context handling) does with any
			// symlink: the link is recorded as-is, and only matters if
			// something later resolves it. Rejecting or rewriting targets
			// here would make a build behave differently against oakd than
			// the same Dockerfile does with a local `docker build`.
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
			}
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("remove existing %s: %w", target, err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s: %w", target, err)
			}
		default:
			return fmt.Errorf("unsupported tar entry type %d for %q", hdr.Typeflag, hdr.Name)
		}
	}
}

// safeExtractPath joins name onto dir after validating that name can't
// place the result outside dir: no absolute path, and no ".." component
// surviving a path.Clean. Tar entry names always use "/" as the separator
// regardless of platform (the tar format's own spec), so this cleans with
// package path, not filepath, before converting to the local OS form.
// Mirrors internal/rootfs's safeJoin, the equivalent guard for the tar
// streams `docker pull` produces when flattening an image's layers.
//
// Also used directly by handleBuild to resolve the X-Oak-Dockerfile header
// against the extracted context dir, since that header is just as
// untrusted as any tar entry name.
func safeExtractPath(dir, name string) (string, error) {
	if path.IsAbs(name) {
		return "", fmt.Errorf("tar entry %q: absolute paths are not allowed", name)
	}
	cleaned := path.Clean(name)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("tar entry %q: escapes build context", name)
	}

	cleanDir := filepath.Clean(dir)
	target := filepath.Join(cleanDir, filepath.FromSlash(cleaned))
	if target != cleanDir && !strings.HasPrefix(target, cleanDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("tar entry %q: escapes build context", name)
	}
	return target, nil
}
