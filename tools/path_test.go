package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// path_test.go exercises path.go, the port of
// packages/agent/src/harness/tools/path-utils.ts @0.82.0.

func TestNormalizeToolPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"no-break space U+00A0", "foo bar.txt", "foo bar.txt"},
		{"en quad U+2000", "foo bar.txt", "foo bar.txt"},
		{"hair space U+200A", "foo bar.txt", "foo bar.txt"},
		{"narrow no-break space U+202F", "foo bar.txt", "foo bar.txt"},
		{"medium mathematical space U+205F", "foo bar.txt", "foo bar.txt"},
		{"ideographic space U+3000", "foo　bar.txt", "foo bar.txt"},
		{"multiple unicode spaces", "a b　c", "a b c"},
		{"strips leading @", "@/foo/bar.txt", "/foo/bar.txt"},
		{"strips leading @ and normalizes spaces", "@foo bar.txt", "foo bar.txt"},
		{"only strips a LEADING @, not one mid-path", "/foo/@bar.txt", "/foo/@bar.txt"},
		{"plain ascii path is unchanged", "/foo/bar baz.txt", "/foo/bar baz.txt"},
		{"empty string", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeToolPath(tc.path)
			if got != tc.want {
				t.Errorf("NormalizeToolPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestResolveToolPath(t *testing.T) {
	env, dir := newTestOSEnv(t)
	ctx := context.Background()

	t.Run("resolves a relative path against Cwd", func(t *testing.T) {
		got, err := ResolveToolPath(ctx, env, "sub/file.txt")
		if err != nil {
			t.Fatalf("ResolveToolPath error: %v", err)
		}
		want := filepath.Join(dir, "sub/file.txt")
		if got != want {
			t.Errorf("ResolveToolPath = %q, want %q", got, want)
		}
	})

	t.Run("normalizes unicode spaces and strips leading @ before resolving", func(t *testing.T) {
		got, err := ResolveToolPath(ctx, env, "@sub dir/file.txt")
		if err != nil {
			t.Fatalf("ResolveToolPath error: %v", err)
		}
		want := filepath.Join(dir, "sub dir/file.txt")
		if got != want {
			t.Errorf("ResolveToolPath = %q, want %q", got, want)
		}
	})

	t.Run("propagates AbsolutePath's ctx-cancellation error", func(t *testing.T) {
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := ResolveToolPath(cctx, env, "x"); err == nil {
			t.Errorf("ResolveToolPath with cancelled ctx error = nil, want non-nil")
		}
	})
}

func TestResolveReadToolPath(t *testing.T) {
	ctx := context.Background()

	t.Run("healing: narrow-no-break-space before PM on disk, asked with an ordinary space", func(t *testing.T) {
		env, dir := newTestOSEnv(t)
		onDisk := "Report 3 PM.txt"
		if err := os.WriteFile(filepath.Join(dir, onDisk), []byte("x"), 0o644); err != nil {
			t.Fatalf("os.WriteFile error: %v", err)
		}

		got, err := ResolveReadToolPath(ctx, env, "Report 3 PM.txt")
		if err != nil {
			t.Fatalf("ResolveReadToolPath error: %v", err)
		}
		want := filepath.Join(dir, onDisk)
		if got != want {
			t.Errorf("ResolveReadToolPath = %q, want %q (the on-disk narrow-no-break-space variant)", got, want)
		}
	})

	t.Run("healing: NFD-decomposed name on disk, asked with the precomposed (NFC) spelling", func(t *testing.T) {
		// Uses fakePathEnv rather than a real filesystem/OSEnv: macOS's
		// HFS+/APFS perform Unicode-normalization-INSENSITIVE filename
		// lookups (the kernel normalizes both the on-disk name and an
		// incoming lookup name before comparing), so a real file stored
		// under its NFD name is already found via a plain os.Lstat of the
		// PRECOMPOSED (NFC) name - that would make a real-filesystem
		// version of this test pass even if ResolveReadToolPath never
		// tried the NFD variant at all, which is not a meaningful test.
		// fakePathEnv's exact string-keyed Exists isolates the actual
		// behavior under test (whether ResolveReadToolPath itself
		// generates and tries the NFD variant) from the host OS's own
		// normalization handling. This is unlike the narrow-no-break-space
		// and curly-quote cases above, which the real filesystem does NOT
		// paper over: those substitute genuinely different, non-
		// canonically-equivalent code points (a narrow space is not
		// Unicode-equivalent to an ASCII space; a curly quote is not
		// Unicode-equivalent to an ASCII apostrophe), so those two cases
		// are legitimately tested against the real OSEnv above.
		precomposed := "/root/café.txt"
		onDisk := norm.NFD.String(precomposed)
		if onDisk == precomposed {
			t.Fatalf("test setup: NFD normalization did not change %q - it must differ from the NFC form for this test to be meaningful", precomposed)
		}
		env := &fakePathEnv{cwd: "/root", existing: map[string]bool{onDisk: true}}

		got, err := ResolveReadToolPath(ctx, env, precomposed)
		if err != nil {
			t.Fatalf("ResolveReadToolPath error: %v", err)
		}
		if got != onDisk {
			t.Errorf("ResolveReadToolPath = %q, want %q (the NFD variant)", got, onDisk)
		}
	})

	t.Run("healing: curly right-single-quote name on disk, asked with an ASCII apostrophe", func(t *testing.T) {
		env, dir := newTestOSEnv(t)
		onDisk := "O’Brien.txt"
		if err := os.WriteFile(filepath.Join(dir, onDisk), []byte("x"), 0o644); err != nil {
			t.Fatalf("os.WriteFile error: %v", err)
		}

		got, err := ResolveReadToolPath(ctx, env, "O'Brien.txt")
		if err != nil {
			t.Fatalf("ResolveReadToolPath error: %v", err)
		}
		want := filepath.Join(dir, onDisk)
		if got != want {
			t.Errorf("ResolveReadToolPath = %q, want %q (the on-disk curly-quote variant)", got, want)
		}
	})

	t.Run("unknown path returns the resolved absolute path unchanged", func(t *testing.T) {
		env, dir := newTestOSEnv(t)
		got, err := ResolveReadToolPath(ctx, env, "totally-missing-file.txt")
		if err != nil {
			t.Fatalf("ResolveReadToolPath error: %v", err)
		}
		want := filepath.Join(dir, "totally-missing-file.txt")
		if got != want {
			t.Errorf("ResolveReadToolPath = %q, want %q (resolved, unhealed)", got, want)
		}
	})

	t.Run("existing path is returned as-is without checking any variant", func(t *testing.T) {
		env, dir := newTestOSEnv(t)
		present := "present.txt"
		if err := os.WriteFile(filepath.Join(dir, present), []byte("x"), 0o644); err != nil {
			t.Fatalf("os.WriteFile error: %v", err)
		}
		got, err := ResolveReadToolPath(ctx, env, present)
		if err != nil {
			t.Fatalf("ResolveReadToolPath error: %v", err)
		}
		want := filepath.Join(dir, present)
		if got != want {
			t.Errorf("ResolveReadToolPath = %q, want %q", got, want)
		}
	})
}

// fakePathEnv is a minimal ExecutionEnv double, implementing only enough of
// the interface for AbsolutePath/Exists-driven tests to type-check: its
// filesystem-touching methods beyond those two panic if ever called.
// AbsolutePath performs the same pure path-joining logic as OSEnv, but
// Exists is an exact string-keyed lookup with no OS involvement at all - see
// its use above for why that matters for the NFD healing case specifically.
type fakePathEnv struct {
	cwd      string
	existing map[string]bool
}

func (e *fakePathEnv) Cwd() string { return e.cwd }

func (e *fakePathEnv) AbsolutePath(ctx context.Context, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Clean(filepath.Join(e.cwd, path)), nil
}

func (e *fakePathEnv) CanonicalPath(ctx context.Context, path string) (string, error) {
	panic("fakePathEnv: CanonicalPath not implemented (unused by path.go)")
}

func (e *fakePathEnv) Exists(ctx context.Context, path string) (bool, error) {
	return e.existing[path], nil
}

func (e *fakePathEnv) FileInfo(ctx context.Context, path string) (FileInfo, error) {
	panic("fakePathEnv: FileInfo not implemented (unused by path.go)")
}

func (e *fakePathEnv) ReadFile(ctx context.Context, path string) ([]byte, error) {
	panic("fakePathEnv: ReadFile not implemented (unused by path.go)")
}

func (e *fakePathEnv) WriteFile(ctx context.Context, path string, data []byte) error {
	panic("fakePathEnv: WriteFile not implemented (unused by path.go)")
}

func (e *fakePathEnv) AppendFile(ctx context.Context, path string, data []byte) error {
	panic("fakePathEnv: AppendFile not implemented (unused by path.go)")
}

func (e *fakePathEnv) CreateTemp(ctx context.Context, prefix, suffix string) (string, error) {
	panic("fakePathEnv: CreateTemp not implemented (unused by path.go)")
}

func (e *fakePathEnv) Exec(ctx context.Context, spec ExecSpec) (*ExecResult, error) {
	panic("fakePathEnv: Exec not implemented (unused by path.go)")
}
