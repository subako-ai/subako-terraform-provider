// Package bundle packs a skill bundle directory into the gzipped tar the
// skill upload routes take, and fingerprints a bundle so a local directory
// and a stored version compare without downloading either.
//
// What goes into the archive mirrors what `subako skill push` packs: every
// regular file under the directory under its "/"-joined relative path, a
// symlink refused rather than followed, anything else passed over, the same
// caps, and headers fixed so the same content always packs to the same bytes.
// The fingerprint below has no counterpart there; it is what lets this
// provider plan without downloading a stored version.
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Limits a bundle must keep, as the server enforces them.
const (
	// CODESYNC(skill-bundle-caps)
	MaxFileBytes = 10 * 1024 * 1024
	// CODESYNC(skill-bundle-caps)
	MaxTotalBytes = 10 * 1024 * 1024
	// CODESYNC(skill-bundle-caps)
	MaxFiles = 1000
)

// CODESYNC(skill-manifest-file)
const skillMD = "SKILL.md"

// File is one file a bundle carries, and its content digest.
type File struct {
	Path   string
	Sha256 string
	bytes  []byte
}

// Bundle is a directory read into memory, files sorted by path.
type Bundle struct {
	Files []File
}

// Read walks dir into a Bundle and checks it against the limits. dir itself
// may be a symlink; nothing under it may. The caps are checked in the order
// the server checks them -- each file's own size, then the
// file count, then the total -- so a directory over more than one of them
// draws the same answer here as from the server.
func Read(dir string) (*Bundle, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("reading skill directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("reading skill directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	walked, err := walk(dir)
	if err != nil {
		return nil, err
	}
	if len(walked) > MaxFiles {
		return nil, fmt.Errorf("%s holds more than the %d files a bundle may carry", dir, MaxFiles)
	}
	var measured int64
	for _, file := range walked {
		measured += file.size
	}
	if measured > MaxTotalBytes {
		return nil, overTotal(dir)
	}

	files := make([]File, 0, len(walked))
	var read int64
	for _, file := range walked {
		// Bounded by the per-file cap, so a file that grew since the walk
		// measured it is refused rather than read whole.
		content, err := readAtMost(file.path, MaxFileBytes+1)
		if err != nil {
			return nil, err
		}
		size := int64(len(content))
		if size > MaxFileBytes {
			return nil, overFile(file.path)
		}
		read += size
		if read > MaxTotalBytes {
			return nil, overTotal(dir)
		}
		digest := sha256.Sum256(content)
		files = append(files, File{Path: file.rel, Sha256: hex.EncodeToString(digest[:]), bytes: content})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if !containsPath(files, skillMD) {
		return nil, fmt.Errorf("%s holds no %s: a skill bundle is the directory that file sits in", dir, skillMD)
	}
	return &Bundle{Files: files}, nil
}

// walkedFile is one file the walk met: where it is, what a bundle calls it,
// and how big it was then.
type walkedFile struct {
	path string
	rel  string
	size int64
}

// walk lists the regular files under dir, refusing a symlink and a file over
// the per-file cap as it meets them. Sizes come from the directory entry, so
// the per-file cap is answered for every file, whatever the others spend.
func walk(dir string) ([]walkedFile, error) {
	var files []walkedFile
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink, which a bundle may not contain", path)
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxFileBytes {
			return overFile(path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, walkedFile{path: path, rel: filepath.ToSlash(rel), size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func overFile(path string) error {
	return fmt.Errorf("%s is more than the %d bytes one file may carry", path, MaxFileBytes)
}

func overTotal(dir string) error {
	return fmt.Errorf("%s holds more than the %d bytes a bundle may carry", dir, MaxTotalBytes)
}

// readAtMost reads up to limit bytes of path.
func readAtMost(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit))
}

func containsPath(files []File, path string) bool {
	for _, f := range files {
		if f.Path == path {
			return true
		}
	}
	return false
}

// Hash fingerprints the bundle's content: its paths and their digests.
func (b *Bundle) Hash() string {
	entries := make([]Entry, len(b.Files))
	for i, f := range b.Files {
		entries[i] = Entry{Path: f.Path, Sha256: f.Sha256}
	}
	return Hash(entries)
}

// Entry is what a fingerprint reads from a file: a stored version's manifest
// carries exactly this.
type Entry struct {
	Path   string
	Sha256 string
}

// Hash fingerprints a set of files, whatever order they come in.
func Hash(entries []Entry) string {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, e := range sorted {
		fmt.Fprintf(h, "%s\x00%s\n", e.Path, e.Sha256)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Pack builds the archive. Every header's mtime is zero and its mode fixed,
// so the bytes depend on the bundle's content and nothing else.
func (b *Bundle) Pack() ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range b.Files {
		header := &tar.Header{
			Name:     f.Path,
			Mode:     0o644,
			Size:     int64(len(f.bytes)),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatGNU,
		}
		if err := tw.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("packing %s: %w", f.Path, err)
		}
		if _, err := tw.Write(f.bytes); err != nil {
			return nil, fmt.Errorf("packing %s: %w", f.Path, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("packing the bundle: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("packing the bundle: %w", err)
	}
	return buf.Bytes(), nil
}
