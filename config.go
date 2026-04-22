package main

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repo struct {
	URL  string `json:"url"`
	Path string `json:"path"`
}

type Config struct {
	Repos []Repo `json:"repos"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "law.json"), nil
}

func loadConfig() (*Config, string, error) {
	path, err := configPath()
	if err != nil {
		return nil, "", err
	}
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, path, nil
		}
		return nil, path, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, path, err
	}
	return cfg, path, nil
}

func saveConfig(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (c *Config) hasRepoURL(u string) bool {
	return c.findRepoByURL(u) != nil
}

func (c *Config) findRepoByURL(u string) *Repo {
	n := normalizeURL(u)
	for i, r := range c.Repos {
		if normalizeURL(r.URL) == n {
			return &c.Repos[i]
		}
	}
	return nil
}

// canonicalizeURL strips the ".git" suffix and rewrites SSH remotes into HTTPS form
// so different notations for the same repo compare structurally.
func canonicalizeURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, ".git")
	if strings.HasPrefix(s, "git@") {
		if i := strings.Index(s, ":"); i > 0 {
			host := strings.TrimPrefix(s[:i], "git@")
			s = "https://" + host + "/" + s[i+1:]
		}
	}
	return strings.TrimSuffix(s, "/")
}

func normalizeURL(raw string) string {
	return strings.ToLower(canonicalizeURL(raw))
}

func (r Repo) ownerRepo() (string, string, bool) {
	u, err := url.Parse(canonicalizeURL(r.URL))
	if err != nil || u.Host != "github.com" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func detectLocalRepo() (Repo, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return Repo{}, false
	}
	if _, err := os.Stat(filepath.Join(cwd, ".git", "config")); err != nil {
		return Repo{}, false
	}
	out, err := exec.Command("git", "-C", cwd, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return Repo{}, false
	}
	u := strings.TrimSpace(string(out))
	if u == "" {
		return Repo{}, false
	}
	return Repo{URL: u, Path: cwd}, true
}
