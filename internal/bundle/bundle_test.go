package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	zeroTime = time.Unix(0, 0)
	bTime    = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func skillDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "SKILL.md", "---\nname: docs\ndescription: d\n---\nbody\n")
	write(t, dir, "scripts/run.sh", "echo hi\n")
	write(t, dir, ".hidden", "kept\n")
	return dir
}

// sparse makes a file of size bytes without writing them.
func sparse(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestReadListsEveryRegularFileSortedWithSlashPaths(t *testing.T) {
	b, err := Read(skillDir(t))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range b.Files {
		paths = append(paths, f.Path)
	}
	if got, want := strings.Join(paths, ","), ".hidden,SKILL.md,scripts/run.sh"; got != want {
		t.Fatalf("paths = %s, want %s", got, want)
	}
	if b.Files[2].Sha256 != digest("echo hi\n") {
		t.Fatalf("digest = %s", b.Files[2].Sha256)
	}
}

func TestHashMatchesAManifestInAnyOrder(t *testing.T) {
	b, err := Read(skillDir(t))
	if err != nil {
		t.Fatal(err)
	}
	manifest := []Entry{
		{Path: "scripts/run.sh", Sha256: digest("echo hi\n")},
		{Path: "SKILL.md", Sha256: digest("---\nname: docs\ndescription: d\n---\nbody\n")},
		{Path: ".hidden", Sha256: digest("kept\n")},
	}
	if b.Hash() != Hash(manifest) {
		t.Fatalf("local %s != manifest %s", b.Hash(), Hash(manifest))
	}
	if !strings.HasPrefix(b.Hash(), "sha256:") {
		t.Fatalf("hash %s lacks its prefix", b.Hash())
	}
}

func TestHashChangesWithContentAndPath(t *testing.T) {
	base := Hash([]Entry{{Path: "SKILL.md", Sha256: "a"}})
	for _, other := range [][]Entry{
		{{Path: "SKILL.md", Sha256: "b"}},
		{{Path: "skill.md", Sha256: "a"}},
		{{Path: "SKILL.md", Sha256: "a"}, {Path: "x", Sha256: "a"}},
	} {
		if Hash(other) == base {
			t.Fatalf("%v hashes like the base", other)
		}
	}
}

func TestPackIsDeterministicAndHoldsEveryFile(t *testing.T) {
	dir := skillDir(t)
	b, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := b.Pack()
	if err != nil {
		t.Fatal(err)
	}
	// A touched file packs to the same bytes: nothing but content counts.
	if err := os.Chtimes(filepath.Join(dir, "SKILL.md"), bTime, bTime); err != nil {
		t.Fatal(err)
	}
	again, _ := Read(dir)
	second, err := again.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("packing the same directory twice gave different bytes")
	}

	gz, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Mode != 0o644 || !h.ModTime.Equal(zeroTime) {
			t.Fatalf("%s has mode %o and mtime %v", h.Name, h.Mode, h.ModTime)
		}
		content, _ := io.ReadAll(tr)
		got[h.Name] = string(content)
	}
	if got["scripts/run.sh"] != "echo hi\n" || len(got) != 3 {
		t.Fatalf("archive holds %v", got)
	}
}

func TestReadRefusesWhatABundleMayNotBe(t *testing.T) {
	noSkill := t.TempDir()
	write(t, noSkill, "README.md", "x")

	withLink := skillDir(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(withLink, "passwd")); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(t.TempDir(), "file")
	write(t, filepath.Dir(file), "file", "x")

	for name, dir := range map[string]string{
		"no SKILL.md": noSkill,
		"symlink":     withLink,
		"not a dir":   file,
		"missing":     filepath.Join(t.TempDir(), "absent"),
	} {
		if _, err := Read(dir); err == nil {
			t.Errorf("%s: Read succeeded", name)
		}
	}
}

func TestReadRefusesADirectoryPastTheCaps(t *testing.T) {
	tooMany := skillDir(t)
	for i := range MaxFiles {
		write(t, tooMany, fmt.Sprintf("f%d", i), "x")
	}

	tooHeavy := skillDir(t)
	half := make([]byte, MaxTotalBytes/2+1)
	for _, rel := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(tooHeavy, rel), half, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	oneTooBig := t.TempDir()
	write(t, oneTooBig, "SKILL.md", "x")
	sparse(t, filepath.Join(oneTooBig, "big"), MaxFileBytes+1)

	// The per-file cap is answered for every file, whatever the ones walked
	// before it have spent, as the server answers it: `a` is walked first
	// and leaves almost none of the total.
	oneTooBigLast := t.TempDir()
	write(t, oneTooBigLast, "SKILL.md", "x")
	sparse(t, filepath.Join(oneTooBigLast, "a"), MaxTotalBytes-1)
	sparse(t, filepath.Join(oneTooBigLast, "z"), MaxFileBytes+1)

	for name, tc := range map[string]struct{ dir, refusal string }{
		"too many files":              {tooMany, "files a bundle may carry"},
		"too many bytes":              {tooHeavy, "bytes a bundle may carry"},
		"one file too big":            {oneTooBig, "bytes one file may carry"},
		"one file too big, last read": {oneTooBigLast, "bytes one file may carry"},
	} {
		_, err := Read(tc.dir)
		if err == nil {
			t.Errorf("%s: Read succeeded", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("%s: refused with %q, want it to name %q", name, err, tc.refusal)
		}
	}

	// One file under each cap is still read.
	atCap := t.TempDir()
	write(t, atCap, "SKILL.md", "x")
	for i := range MaxFiles - 1 {
		write(t, atCap, fmt.Sprintf("f%d", i), "x")
	}
	b, err := Read(atCap)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) != MaxFiles {
		t.Fatalf("read %d files, want %d", len(b.Files), MaxFiles)
	}
}

func TestTheRootMayBeASymlink(t *testing.T) {
	dir := skillDir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	viaLink, err := Read(link)
	if err != nil {
		t.Fatal(err)
	}
	direct, _ := Read(dir)
	if viaLink.Hash() != direct.Hash() {
		t.Fatal("a symlinked root hashed differently")
	}
}
