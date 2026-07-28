package tools

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
)

//go:embed manifest.json
var embeddedManifest []byte

// Manifest is the release-pinned tools catalog.
type Manifest struct {
	Version  string                    `json:"version"`
	Profiles map[string]ProfileManifest `json:"profiles"`
}

// ProfileManifest describes one managed language server profile.
type ProfileManifest struct {
	Kind           string            `json:"kind"`
	Packages       []string          `json:"packages,omitempty"`
	Package        string            `json:"package,omitempty"`
	Version        string            `json:"version,omitempty"`
	Bin            string            `json:"bin,omitempty"`
	Args           []string          `json:"args,omitempty"`
	FileExtensions []string          `json:"file_extensions,omitempty"`
	RootMarkers    []string          `json:"root_markers,omitempty"`
	TSServerRel    string            `json:"tsserver_rel,omitempty"`
	SharedInstall  string            `json:"shared_install,omitempty"`
	Repo           string            `json:"repo,omitempty"`
	Tag            string            `json:"tag,omitempty"`
	Assets         map[string]Asset  `json:"assets,omitempty"`
	Disabled       bool              `json:"disabled,omitempty"`
	DisabledReason string            `json:"disabled_reason,omitempty"`
}

// Asset is one platform-specific download target.
type Asset struct {
	Name       string   `json:"name"`
	URLs       []string `json:"urls"`
	SHA256     string   `json:"sha256"`
	ExtractBin string   `json:"extract_bin,omitempty"`
}

// LoadManifest reads LANGER_TOOLS_MANIFEST when set, otherwise the embedded release manifest.
func LoadManifest() (*Manifest, error) {
	if path := os.Getenv(EnvManifestPath); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("tools manifest %s: %w", path, err)
		}
		return parseManifest(data)
	}
	return parseManifest(embeddedManifest)
}

func parseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("tools manifest: %w", err)
	}
	if m.Profiles == nil {
		m.Profiles = map[string]ProfileManifest{}
	}
	return &m, nil
}

// ProfileIDs returns sorted profile names.
func (m *Manifest) ProfileIDs() []string {
	ids := make([]string, 0, len(m.Profiles))
	for id := range m.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Profile returns a named profile.
func (m *Manifest) Profile(id string) (ProfileManifest, bool) {
	p, ok := m.Profiles[id]
	return p, ok
}

// ProfileForExtension finds the first enabled profile claiming ext (e.g. ".ts").
func (m *Manifest) ProfileForExtension(ext string) (id string, p ProfileManifest, ok bool) {
	ext = strings.ToLower(ext)
	if ext == "" {
		return "", ProfileManifest{}, false
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	for _, id := range m.ProfileIDs() {
		p := m.Profiles[id]
		if p.Disabled {
			continue
		}
		for _, e := range p.FileExtensions {
			if strings.ToLower(e) == ext {
				return id, p, true
			}
		}
	}
	return "", ProfileManifest{}, false
}

// PlatformKey is "goos/goarch" for asset lookup (e.g. windows/amd64).
func PlatformKey() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// AssetForPlatform returns the asset for the current platform, if any.
func (p ProfileManifest) AssetForPlatform() (Asset, bool) {
	if p.Assets == nil {
		return Asset{}, false
	}
	a, ok := p.Assets[PlatformKey()]
	return a, ok
}
