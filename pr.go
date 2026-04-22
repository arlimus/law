package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type prRef struct {
	owner  string
	repo   string
	number int
}

func (p prRef) String() string {
	return fmt.Sprintf("%s/%s#%d", p.owner, p.repo, p.number)
}

func (p prRef) repoURL() string {
	return fmt.Sprintf("https://github.com/%s/%s", p.owner, p.repo)
}

func (p prRef) htmlURL() string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", p.owner, p.repo, p.number)
}

func parsePR(raw string) (prRef, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return prRef{}, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Host != "github.com" {
		return prRef{}, fmt.Errorf("not a github.com URL: %s", u.Host)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return prRef{}, fmt.Errorf("expected /<owner>/<repo>/pull/<number>, got %s", u.Path)
	}

	n, err := strconv.Atoi(parts[3])
	if err != nil {
		return prRef{}, fmt.Errorf("invalid PR number %q: %w", parts[3], err)
	}

	return prRef{owner: parts[0], repo: parts[1], number: n}, nil
}
