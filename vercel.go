package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

type vercelPreview struct {
	Name string
	URL  string
}

// previewIcon is the Nerd Fonts FontAwesome external-link glyph (nf-fa-external_link, U+F08E).
const previewIcon = ""

const vercelBotLogin = "vercel[bot]"

var (
	// Current format: | [name](https://vercel.com/...) | ... | [Preview](url) | ... |
	vercelNameRe = regexp.MustCompile(`\[([^\]]+)\]\(https://vercel\.com/`)
	vercelURLRe  = regexp.MustCompile(`\[Preview\]\(([^)]+)\)`)
	// Legacy format: | **name** | ... | [Visit Preview](url) | ... |
	vercelLegacyNameRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	vercelLegacyURLRe  = regexp.MustCompile(`\[Visit Preview\]\(([^)]+)\)`)
)

// fetchVercelPreviews scrapes preview URLs from the latest vercel[bot] comments
// on the PR. Returns nil if gh isn't available or the comment isn't found.
func fetchVercelPreviews(owner, name string, prNumber int) []vercelPreview {
	endpoint := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100", owner, name, prNumber)
	out, err := exec.Command("gh", "api", "--paginate", endpoint).Output()
	if err != nil {
		return nil
	}
	var raw []struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var previews []vercelPreview
	for _, c := range raw {
		if c.User.Login != vercelBotLogin {
			continue
		}
		for _, p := range parseVercelComment(c.Body) {
			key := p.Name + "|" + p.URL
			if seen[key] {
				continue
			}
			seen[key] = true
			previews = append(previews, p)
		}
	}
	return previews
}

func parseVercelComment(body string) []vercelPreview {
	var previews []vercelPreview
	for line := range strings.SplitSeq(body, "\n") {
		var name, url string
		if u := vercelURLRe.FindStringSubmatch(line); u != nil {
			url = strings.TrimSpace(u[1])
			if n := vercelNameRe.FindStringSubmatch(line); n != nil {
				name = strings.TrimSpace(n[1])
			}
		}
		if url == "" {
			if u := vercelLegacyURLRe.FindStringSubmatch(line); u != nil {
				url = strings.TrimSpace(u[1])
				if n := vercelLegacyNameRe.FindStringSubmatch(line); n != nil {
					name = strings.TrimSpace(n[1])
				}
			}
		}
		if name == "" || url == "" {
			continue
		}
		previews = append(previews, vercelPreview{Name: name, URL: url})
	}
	return previews
}
