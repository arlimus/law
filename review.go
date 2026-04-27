package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type pullRequest struct {
	Number    int
	Title     string
	Author    string
	CreatedAt time.Time
	InReview  bool
}

// humanizeAgo renders a past duration as a short relative string like "3d" or "2mo".
func humanizeAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := max(time.Since(t), 0)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dw", int(d/(7*24*time.Hour)))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d/(30*24*time.Hour)))
	default:
		return fmt.Sprintf("%dy", int(d/(365*24*time.Hour)))
	}
}

const reviewPrefix = ".review"

func parseReviewDir(name string) (int, bool) {
	if !strings.HasPrefix(name, reviewPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(name[len(reviewPrefix):])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func inProgressReviews(repoPath string) []int {
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil
	}
	var nums []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if n, ok := parseReviewDir(e.Name()); ok {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	return nums
}

func fetchPRs(repo *Repo) ([]pullRequest, error) {
	owner, name, ok := repo.ownerRepo()
	if !ok {
		return nil, fmt.Errorf("not a github repo URL: %s", repo.URL)
	}

	endpoint := fmt.Sprintf("repos/%s/%s/pulls?state=open&per_page=100", owner, name)
	cmd := exec.Command("gh", "api", "--paginate", endpoint)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("gh api failed: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("running gh: %w", err)
	}

	var raw []struct {
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		CreatedAt time.Time `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing gh output: %w", err)
	}

	active := make(map[int]bool)
	for _, n := range inProgressReviews(repo.Path) {
		active[n] = true
	}

	prs := make([]pullRequest, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, pullRequest{
			Number:    r.Number,
			Title:     r.Title,
			Author:    r.User.Login,
			CreatedAt: r.CreatedAt,
			InReview:  active[r.Number],
		})
	}
	return prs, nil
}

func reviewPath(repo *Repo, prNumber int) string {
	return filepath.Join(repo.Path, fmt.Sprintf("%s%d", reviewPrefix, prNumber))
}

func startReview(repo *Repo, prNumber int) (string, error) {
	if info, err := os.Stat(repo.Path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("repo path missing: %s", repo.Path)
	}

	path := reviewPath(repo, prNumber)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path, nil
	}

	clone := exec.Command("git", "clone", repo.URL, path)
	if out, err := clone.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone: %v\n%s", err, strings.TrimSpace(string(out)))
	}

	checkout := exec.Command("gh", "pr", "checkout", strconv.Itoa(prNumber))
	checkout.Dir = path
	if out, err := checkout.CombinedOutput(); err != nil {
		return path, fmt.Errorf("gh pr checkout: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
}
