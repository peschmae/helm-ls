package plato

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.lsp.dev/uri"
	"gopkg.in/yaml.v3"
)

type settings struct {
	Root             string              `json:"root"`
	ConfigPath       string              `json:"configPath"`
	Source           string              `json:"source"`
	ExcludedPaths    []string            `json:"excludedPaths"`
	BinaryExtensions []string            `json:"binaryExtensions"`
	YAMLBackend      string              `json:"yamlBackend"`
	BashBackend      string              `json:"bashBackend"`
	ShellCheckPath   string              `json:"shellcheckPath"`
	YAMLSchemas      map[string][]string `json:"yamlSchemas"`
}

type workspace struct {
	root, source, left, right, problem string
	excluded, binary                   []string
}

func (s settings) forFile(filename string, folders []string) workspace {
	filename, _ = filepath.Abs(filename)
	root := s.Root
	if root == "" {
		for _, folder := range folders {
			folder, _ = filepath.Abs(folder)
			if inside(folder, filename) && len(folder) > len(root) {
				root = folder
			}
		}
	}
	if root == "" {
		if len(folders) > 0 && s.Root == "" && s.Source == "" && s.ConfigPath == "" {
			return workspace{}
		}
		root = filepath.Dir(filename)
	}
	root, _ = filepath.Abs(root)
	configPath := s.ConfigPath
	if configPath == "" {
		configPath = "plato.yaml"
	}
	if s.Root == "" && s.ConfigPath == "" {
		limit := root
		if len(folders) == 0 {
			limit = filepath.VolumeName(root) + string(filepath.Separator)
		}
		for dir := filepath.Dir(filename); inside(limit, dir) || dir == limit; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, configPath)); err == nil {
				root = dir
				break
			}
			if dir == limit || dir == filepath.Dir(dir) {
				break
			}
		}
	}
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(root, configPath)
	}
	var config struct {
		Plato struct {
			Source           string   `yaml:"source"`
			BinaryExtensions []string `yaml:"binary_extensions"`
			Delimiters       struct {
				Left  string `yaml:"left"`
				Right string `yaml:"right"`
			} `yaml:"delimiters"`
		} `yaml:"plato"`
	}
	w := workspace{root: root, source: "templates", left: "{{{", right: "}}}", binary: []string{".tar.gz", ".gz", ".tgz", ".zip", ".gem"}}
	if data, err := os.ReadFile(configPath); err == nil {
		if yaml.Unmarshal(data, &config) != nil {
			w.problem = "Cannot read Plato workspace configuration"
		} else {
			if config.Plato.Source != "" {
				w.source = config.Plato.Source
			}
			if config.Plato.Delimiters.Left != "" {
				w.left = config.Plato.Delimiters.Left
			}
			if config.Plato.Delimiters.Right != "" {
				w.right = config.Plato.Delimiters.Right
			}
			if len(config.Plato.BinaryExtensions) > 0 {
				w.binary = config.Plato.BinaryExtensions
			}
		}
	} else if !os.IsNotExist(err) {
		w.problem = "Cannot read Plato workspace configuration"
	} else if s.ConfigPath != "" {
		w.problem = "Cannot find Plato workspace configuration"
	} else if s.Root == "" && s.Source == "" {
		// Without an explicit root, only a real Plato project is eligible.
		return workspace{}
	}
	if s.Source != "" {
		w.source = s.Source
	}
	if len(s.BinaryExtensions) > 0 {
		w.binary = s.BinaryExtensions
	}
	w.excluded = s.ExcludedPaths
	if !filepath.IsAbs(w.source) {
		w.source = filepath.Join(root, w.source)
	}
	w.source = filepath.Clean(w.source)
	return w
}

func inside(root, filename string) bool {
	rel, err := filepath.Rel(root, filename)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w workspace) eligible(filename, content string) bool {
	if w.source == "" || !inside(w.source, filename) || strings.ContainsRune(content, 0) {
		return false
	}
	rel, _ := filepath.Rel(w.source, filename)
	for _, suffix := range append(append([]string{}, w.binary...), ".sops_enc", ".symlink") {
		if strings.HasSuffix(filename, suffix) {
			return false
		}
	}
	for _, pattern := range w.excluded {
		if match, _ := filepath.Match(pattern, filepath.ToSlash(rel)); match {
			return false
		}
	}
	if info, err := os.Lstat(filename); err == nil && !info.Mode().IsRegular() {
		return false
	}
	if _, err := os.Lstat(filename + ".symlink"); err == nil {
		return false
	}
	return true
}

func pathForURI(u uri.URI) string {
	if !strings.HasPrefix(string(u), "file://") {
		return ""
	}
	return u.Filename()
}

func parseSettings(value any) (settings, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return settings{}, errors.New("invalid Plato language server settings")
	}
	var s settings
	if err := json.Unmarshal(data, &s); err != nil {
		return settings{}, errors.New("invalid Plato language server settings")
	}
	var provided map[string]json.RawMessage
	if err := json.Unmarshal(data, &provided); err != nil {
		return settings{}, errors.New("invalid Plato language server settings")
	}
	if _, present := provided["yamlBackend"]; !present {
		s.YAMLBackend = os.Getenv("PLATO_LS_YAML_BACKEND")
	}
	if _, present := provided["bashBackend"]; !present {
		s.BashBackend = os.Getenv("PLATO_LS_BASH_BACKEND")
	}
	if _, present := provided["shellcheckPath"]; !present {
		s.ShellCheckPath = os.Getenv("PLATO_LS_SHELLCHECK_PATH")
	}
	for schema := range s.YAMLSchemas {
		u, err := url.Parse(schema)
		if err != nil || !strings.HasPrefix(schema, "file:///") ||
			u.Scheme != "file" || u.Host != "" || !filepath.IsAbs(u.Path) {
			return settings{}, errors.New("YAML schemas must use absolute local file:// URIs")
		}
	}
	return s, nil
}
