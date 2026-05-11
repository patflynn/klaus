package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")

	reg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom non-existent file: %v", err)
	}
	if len(reg.Projects) != 0 {
		t.Errorf("expected empty projects map, got %d entries", len(reg.Projects))
	}
	if reg.ProjectsDir == "" {
		t.Error("expected default ProjectsDir, got empty string")
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")

	reg := &Registry{
		ProjectsDir: "/home/test/projects",
		Projects:    map[string]string{"myrepo": "/home/test/projects/myrepo"},
	}

	if err := reg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if loaded.ProjectsDir != reg.ProjectsDir {
		t.Errorf("ProjectsDir = %q, want %q", loaded.ProjectsDir, reg.ProjectsDir)
	}
	if len(loaded.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(loaded.Projects))
	}
	if loaded.Projects["myrepo"] != "/home/test/projects/myrepo" {
		t.Errorf("myrepo path = %q, want %q", loaded.Projects["myrepo"], "/home/test/projects/myrepo")
	}
}

func TestAddAndGet(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects:    make(map[string]string),
	}

	if err := reg.Add("foo", "/tmp/projects/foo"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	path, ok := reg.Get("foo")
	if !ok {
		t.Fatal("Get returned false for existing project")
	}
	if path != "/tmp/projects/foo" {
		t.Errorf("Get = %q, want %q", path, "/tmp/projects/foo")
	}
}

func TestAddDuplicateReturnsError(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects:    map[string]string{"foo": "/tmp/foo"},
	}

	err := reg.Add("foo", "/tmp/other/foo")
	if err == nil {
		t.Fatal("expected error for duplicate name, got nil")
	}
}

func TestRemove(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects:    map[string]string{"foo": "/tmp/foo"},
	}

	if err := reg.Remove("foo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, ok := reg.Get("foo")
	if ok {
		t.Error("Get returned true after Remove")
	}
}

func TestRemoveNonExistent(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects:    make(map[string]string),
	}

	err := reg.Remove("nope")
	if err == nil {
		t.Fatal("expected error for non-existent project, got nil")
	}
}

func TestGetNonExistent(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects:    make(map[string]string),
	}

	_, ok := reg.Get("nope")
	if ok {
		t.Error("Get returned true for non-existent project")
	}
}

func TestList(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects: map[string]string{
			"a": "/tmp/a",
			"b": "/tmp/b",
		},
	}

	list := reg.List()
	if len(list) != 2 {
		t.Fatalf("List returned %d entries, want 2", len(list))
	}
	if list["a"] != "/tmp/a" {
		t.Errorf("list[a] = %q, want %q", list["a"], "/tmp/a")
	}
	if list["b"] != "/tmp/b" {
		t.Errorf("list[b] = %q, want %q", list["b"], "/tmp/b")
	}
}

func TestSetProjectsDir(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/old",
		Projects:    make(map[string]string),
	}

	reg.SetProjectsDir("/new/path")
	if reg.ProjectsDir != "/new/path" {
		t.Errorf("ProjectsDir = %q, want %q", reg.ProjectsDir, "/new/path")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"tilde only", "~", home},
		{"tilde slash", "~/foo/bar", filepath.Join(home, "foo/bar")},
		{"absolute", "/usr/bin", "/usr/bin"},
		{"relative", "foo/bar", "foo/bar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandHome(tt.input)
			if err != nil {
				t.Fatalf("ExpandHome(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ExpandHome(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSaveContractsHomePaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")

	reg := &Registry{
		ProjectsDir: filepath.Join(home, "projects"),
		Projects:    map[string]string{"foo": filepath.Join(home, "projects/foo")},
	}

	if err := reg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	// Read raw JSON to verify ~ is stored
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	raw := string(data)
	if filepath.Join(home, "projects") != "" {
		// The file should contain ~/projects, not the expanded path
		if !contains(raw, "~/projects") {
			t.Errorf("saved file should contain ~/projects, got:\n%s", raw)
		}
	}
}

func TestRoundTripWithTildePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")

	// Write a file with tilde paths
	if err := os.WriteFile(path, []byte(`{
		"projects_dir": "~/hack",
		"projects": {"klaus": "~/hack/klaus"}
	}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}

	// Get should expand ~
	p, ok := reg.Get("klaus")
	if !ok {
		t.Fatal("Get returned false")
	}
	if p != filepath.Join(home, "hack/klaus") {
		t.Errorf("Get = %q, want %q", p, filepath.Join(home, "hack/klaus"))
	}

	// ExpandedProjectsDir should expand ~
	pd, err := reg.ExpandedProjectsDir()
	if err != nil {
		t.Fatalf("ExpandedProjectsDir: %v", err)
	}
	if pd != filepath.Join(home, "hack") {
		t.Errorf("ExpandedProjectsDir = %q, want %q", pd, filepath.Join(home, "hack"))
	}
}

func TestDescribeAndDescription(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects:    map[string]string{"foo": "/tmp/foo"},
	}

	if got := reg.Description("foo"); got != "" {
		t.Errorf("Description before set = %q, want empty", got)
	}

	if err := reg.Describe("foo", "the foo tool"); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := reg.Description("foo"); got != "the foo tool" {
		t.Errorf("Description = %q, want %q", got, "the foo tool")
	}
}

func TestDescribeUnregisteredProjectErrors(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects:    make(map[string]string),
	}

	if err := reg.Describe("nope", "anything"); err == nil {
		t.Fatal("expected error for unregistered project, got nil")
	}
}

func TestDescribeNormalizesWhitespace(t *testing.T) {
	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects:    map[string]string{"foo": "/tmp/foo"},
	}

	if err := reg.Describe("foo", "  the\tfoo\n tool  "); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := reg.Description("foo"); got != "the foo tool" {
		t.Errorf("Description = %q, want %q", got, "the foo tool")
	}

	// Whitespace-only input should be treated as empty and clear the entry.
	if err := reg.Describe("foo", "   \t\n  "); err != nil {
		t.Fatalf("Describe whitespace-only: %v", err)
	}
	if _, ok := reg.Descriptions["foo"]; ok {
		t.Error("expected whitespace-only description to clear entry")
	}
}

func TestDescribeEmptyClearsEntry(t *testing.T) {
	reg := &Registry{
		ProjectsDir:  "/tmp/projects",
		Projects:     map[string]string{"foo": "/tmp/foo"},
		Descriptions: map[string]string{"foo": "old"},
	}

	if err := reg.Describe("foo", ""); err != nil {
		t.Fatalf("Describe with empty: %v", err)
	}
	if _, ok := reg.Descriptions["foo"]; ok {
		t.Error("expected description entry to be deleted on empty input")
	}
	if got := reg.Description("foo"); got != "" {
		t.Errorf("Description after clear = %q, want empty", got)
	}
}

func TestRemoveCleansUpDescription(t *testing.T) {
	reg := &Registry{
		ProjectsDir:  "/tmp/projects",
		Projects:     map[string]string{"foo": "/tmp/foo"},
		Descriptions: map[string]string{"foo": "the foo tool"},
	}

	if err := reg.Remove("foo"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := reg.Descriptions["foo"]; ok {
		t.Error("Remove should also delete the description entry")
	}
}

func TestDescriptionsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")

	reg := &Registry{
		ProjectsDir:  "/tmp/projects",
		Projects:     map[string]string{"foo": "/tmp/foo", "bar": "/tmp/bar"},
		Descriptions: map[string]string{"foo": "foo tool"},
	}

	if err := reg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	loaded, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	if got := loaded.Description("foo"); got != "foo tool" {
		t.Errorf("Description(foo) after round-trip = %q, want %q", got, "foo tool")
	}
	if got := loaded.Description("bar"); got != "" {
		t.Errorf("Description(bar) after round-trip = %q, want empty", got)
	}
}

func TestLoadLegacyJSONWithoutDescriptionsKey(t *testing.T) {
	// Mirror the literal shape of an existing projects.json from before the
	// descriptions field was added — no `descriptions` key at all.
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")

	if err := os.WriteFile(path, []byte(`{
		"projects_dir": "~/hack",
		"projects": {"klaus": "~/hack/klaus"}
	}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom legacy file: %v", err)
	}

	if reg.Descriptions == nil {
		t.Error("Descriptions should be initialized to non-nil empty map for legacy files")
	}
	if got := reg.Description("klaus"); got != "" {
		t.Errorf("Description(klaus) = %q, want empty for legacy file", got)
	}

	// Setting a description on a legacy-loaded registry should still work.
	if err := reg.Describe("klaus", "self-hosting orchestrator"); err != nil {
		t.Fatalf("Describe after legacy load: %v", err)
	}
	if got := reg.Description("klaus"); got != "self-hosting orchestrator" {
		t.Errorf("Description after Describe = %q", got)
	}
}

func TestSaveOmitsDescriptionsKeyWhenEmpty(t *testing.T) {
	// When no descriptions are set, the saved JSON should not include a
	// `descriptions` key (omitempty), keeping diffs clean for users who
	// don't use this feature.
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.json")

	reg := &Registry{
		ProjectsDir: "/tmp/projects",
		Projects:    map[string]string{"foo": "/tmp/foo"},
	}
	if err := reg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if contains(string(data), "descriptions") {
		t.Errorf("saved file should not contain `descriptions` when none set:\n%s", data)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
