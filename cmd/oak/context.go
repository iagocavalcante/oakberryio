package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// tarContext walks root (the build context, normally the current directory
// for `oak deploy --remote`) and tars everything it contains for oakd's
// POST /apps/{name}/build to build against -- see docs/plans/
// 2026-09-15-remote-build-design.md and internal/daemon/api.go's
// handleBuild.
//
// Exclusions come from root/.dockerignore, read and matched with
// github.com/moby/patternmatcher's ignorefile and patternmatcher packages:
// the same libraries `docker build` itself uses, so a pattern that excludes
// a file from a local build excludes it here too. .git and the
// .dockerignore file itself are always excluded, regardless of what
// .dockerignore says, matching `docker build`'s own context rules.
//
// The whole tar is built in memory rather than streamed through a pipe;
// these repos' build contexts are tens of MB (see the design doc's "out of
// scope" section), so that's simpler and plenty fast.
func tarContext(root string) (*bytes.Buffer, error) {
	var patterns []string
	f, err := os.Open(filepath.Join(root, ".dockerignore"))
	switch {
	case err == nil:
		patterns, err = ignorefile.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("read .dockerignore: %w", err)
		}
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("open .dockerignore: %w", err)
	}
	pm, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, fmt.Errorf("parse .dockerignore: %w", err)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", p, err)
		}
		relSlash := filepath.ToSlash(rel)

		if relSlash == ".git" {
			return fs.SkipDir
		}
		if relSlash == ".dockerignore" {
			return nil
		}
		match, err := pm.MatchesOrParentMatches(relSlash)
		if err != nil {
			return fmt.Errorf("match %s against .dockerignore: %w", relSlash, err)
		}
		if match {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		return tarEntry(tw, p, relSlash, d)
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar writer: %w", err)
	}
	return &buf, nil
}

// tarEntry writes one file, directory, or symlink into tw. name is p's
// slash-separated path relative to the build context root.
func tarEntry(tw *tar.Writer, p, name string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("stat %s: %w", p, err)
	}

	var link string
	if info.Mode()&os.ModeSymlink != 0 {
		link, err = os.Readlink(p)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", p, err)
		}
	}

	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("tar header for %s: %w", p, err)
	}
	hdr.Name = name
	if d.IsDir() {
		hdr.Name += "/"
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write tar header for %s: %w", p, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}
	rf, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("open %s: %w", p, err)
	}
	defer rf.Close()
	if _, err := io.Copy(tw, rf); err != nil {
		return fmt.Errorf("write %s to tar: %w", p, err)
	}
	return nil
}
