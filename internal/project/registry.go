package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Registry holds the project registry: a default projects directory
// and a map of project name to local path.
type Registry struct {
	ProjectsDir string            `json:"projects_dir"`
	Projects    map[string]string `json:"projects"`
}

// registryPath returns the path to ~/.klaus/projects.json.
func registryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".klaus", "projects.json"), nil
}

// ExpandHome expands a leading ~ in a path to the user's home directory.
func ExpandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir: %w", err)
		}
		return filepath.Join(home, path[1:]), nil
	}
	return path, nil
}

// contractHome replaces the user's home directory prefix with ~ for storage.
func contractHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// Load reads the registry from ~/.klaus/projects.json.
// Returns an empty registry with defaults if the file doesn't exist.
func Load() (*Registry, error) {
	p, err := registryPath()
	if err != nil {
		return nil, err
	}
	return loadFrom(p)
}

// loadFrom reads a registry from the given path. Exported for testing via LoadFrom.
func loadFrom(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultRegistry()
		}
		return nil, fmt.Errorf("reading projects file: %w", err)
	}

	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parsing projects file: %w", err)
	}
	if reg.Projects == nil {
		reg.Projects = make(map[string]string)
	}
	return &reg, nil
}

// defaultRegistry returns a new registry with sensible defaults.
func defaultRegistry() (*Registry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home dir: %w", err)
	}
	return &Registry{
		ProjectsDir: filepath.Join(home, "src"),
		Projects:    make(map[string]string),
	}, nil
}

// Save writes the registry to ~/.klaus/projects.json.
func (r *Registry) Save() error {
	p, err := registryPath()
	if err != nil {
		return err
	}
	return r.saveTo(p)
}

// saveTo writes the registry to the given path.
func (r *Registry) saveTo(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	// Store paths with ~ for readability
	out := Registry{
		ProjectsDir: contractHome(r.ProjectsDir),
		Projects:    make(map[string]string, len(r.Projects)),
	}
	for name, p := range r.Projects {
		out.Projects[name] = contractHome(p)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling projects file: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

// Add registers a project with the given name and local path.
// The path must be absolute. Returns an error if the name is already registered.
func (r *Registry) Add(name, localPath string) error {
	if _, exists := r.Projects[name]; exists {
		return fmt.Errorf("project %q is already registered", name)
	}
	r.Projects[name] = localPath
	return nil
}

// Remove unregisters a project by name. Returns an error if the name is not found.
func (r *Registry) Remove(name string) error {
	if _, exists := r.Projects[name]; !exists {
		return fmt.Errorf("project %q is not registered", name)
	}
	delete(r.Projects, name)
	return nil
}

// Get returns the local path for a project, expanding ~ if present.
func (r *Registry) Get(name string) (string, bool) {
	p, ok := r.Projects[name]
	if !ok {
		return "", false
	}
	expanded, err := ExpandHome(p)
	if err != nil {
		return p, true
	}
	return expanded, true
}

// List returns all registered projects as a map of name to expanded local path.
func (r *Registry) List() map[string]string {
	result := make(map[string]string, len(r.Projects))
	for name, p := range r.Projects {
		expanded, err := ExpandHome(p)
		if err != nil {
			expanded = p
		}
		result[name] = expanded
	}
	return result
}

// SetProjectsDir sets the default projects directory.
func (r *Registry) SetProjectsDir(dir string) {
	r.ProjectsDir = dir
}

// ExpandedProjectsDir returns the projects directory with ~ expanded.
func (r *Registry) ExpandedProjectsDir() (string, error) {
	return ExpandHome(r.ProjectsDir)
}

// LoadFrom reads a registry from the given path (for testing).
func LoadFrom(path string) (*Registry, error) {
	return loadFrom(path)
}

// SaveTo writes the registry to the given path (for testing).
func (r *Registry) SaveTo(path string) error {
	return r.saveTo(path)
}
