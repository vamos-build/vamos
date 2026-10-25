package features

import (
	"strings"
	"testing"
)

// TestMatchFiles checks the comparison that the image and export steps make, since every scenario relies on it.
func TestMatchFiles(t *testing.T) {
	entries := map[string]entry{
		"a.txt":     {regular: true, mode: 0o644, content: "a\n"},
		"sub":       {dir: true},
		"sub/b.txt": {regular: true, mode: 0o755, content: "b\n"},
	}
	for _, tc := range []struct {
		name    string
		extra   map[string]entry
		want    []map[string]string
		exactly bool
		errs    []string
	}{
		{
			name: "every file listed",
			want: []map[string]string{
				{"path": "a.txt", "content": "a\n", "mode": "0644"},
				{"path": "sub/b.txt", "content": "b\n"},
			},
			exactly: true,
		},
		{
			name:  "an unlisted file",
			extra: map[string]entry{"c.txt": {regular: true}},
			want: []map[string]string{
				{"path": "a.txt", "content": "a\n"},
				{"path": "sub/b.txt", "content": "b\n"},
			},
			exactly: true,
			errs:    []string{"c.txt is there too"},
		},
		{
			name:  "an unlisted file is allowed without exactly",
			extra: map[string]entry{"c.txt": {regular: true}},
			want:  []map[string]string{{"path": "a.txt", "content": "a\n"}},
		},
		{
			name:  "an empty directory",
			extra: map[string]entry{"empty": {dir: true}},
			want: []map[string]string{
				{"path": "a.txt", "content": "a\n"},
				{"path": "sub/b.txt", "content": "b\n"},
			},
			exactly: true,
			errs:    []string{"empty is there too"},
		},
		{
			name:  "a directory whose name prefixes a listed file's",
			extra: map[string]entry{"su": {dir: true}},
			want: []map[string]string{
				{"path": "a.txt", "content": "a\n"},
				{"path": "sub/b.txt", "content": "b\n"},
			},
			exactly: true,
			errs:    []string{"su is there too"},
		},
		{
			name:  "a link",
			extra: map[string]entry{"link": {}},
			want:  []map[string]string{{"path": "link", "content": ""}},
			errs:  []string{"link is not a regular file"},
		},
		{
			name: "wrong content, mode and a missing file, together",
			want: []map[string]string{
				{"path": "a.txt", "content": "A\n", "mode": "0600"},
				{"path": "gone.txt", "content": ""},
				{"path": "sub", "content": ""},
				{"path": "sub/b.txt", "content": "b\n", "mode": "x"},
			},
			errs: []string{
				`a.txt = "a\n", want "A\n"`,
				"mode of a.txt = 0644, want 0600",
				"gone.txt not found",
				"sub is a directory",
				"the mode to expect of sub/b.txt, x",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := map[string]entry{}
			for path, e := range entries {
				all[path] = e
			}
			for path, e := range tc.extra {
				all[path] = e
			}
			err := matchFiles(all, tc.want, tc.exactly)
			if len(tc.errs) == 0 {
				if err != nil {
					t.Fatalf("matchFiles = %v, want no error", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("matchFiles succeeded, want %q", tc.errs)
			}
			for _, want := range tc.errs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("matchFiles = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}
