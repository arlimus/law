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
	Status    string // "open", "draft", "merged", "closed", "unknown"
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
		Draft     bool      `json:"draft"`
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

	seen := make(map[int]bool, len(raw))
	prs := make([]pullRequest, 0, len(raw)+len(active))
	for _, r := range raw {
		seen[r.Number] = true
		status := "open"
		if r.Draft {
			status = "draft"
		}
		prs = append(prs, pullRequest{
			Number:    r.Number,
			Title:     r.Title,
			Author:    r.User.Login,
			CreatedAt: r.CreatedAt,
			InReview:  active[r.Number],
			Status:    status,
		})
	}

	// Local review folders without a matching open PR are old (merged/closed).
	for n := range active {
		if seen[n] {
			continue
		}
		prs = append(prs, fetchClosedPR(owner, name, n))
	}

	sort.Slice(prs, func(i, j int) bool { return prs[i].Number > prs[j].Number })
	return prs, nil
}

func fetchClosedPR(owner, name string, number int) pullRequest {
	cmd := exec.Command("gh", "api", fmt.Sprintf("repos/%s/%s/pulls/%d", owner, name, number))
	out, err := cmd.Output()
	if err != nil {
		return pullRequest{Number: number, Title: "(unavailable)", InReview: true, Status: "unknown"}
	}
	var r struct {
		Number    int        `json:"number"`
		Title     string     `json:"title"`
		CreatedAt time.Time  `json:"created_at"`
		MergedAt  *time.Time `json:"merged_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return pullRequest{Number: number, Title: "(parse error)", InReview: true, Status: "unknown"}
	}
	status := "closed"
	if r.MergedAt != nil {
		status = "merged"
	}
	return pullRequest{
		Number:    r.Number,
		Title:     r.Title,
		Author:    r.User.Login,
		CreatedAt: r.CreatedAt,
		InReview:  true,
		Status:    status,
	}
}

func reviewPath(repo *Repo, prNumber int) string {
	return filepath.Join(repo.Path, fmt.Sprintf("%s%d", reviewPrefix, prNumber))
}

func removeReview(repo *Repo, prNumber int) error {
	path := reviewPath(repo, prNumber)
	remove := exec.Command("git", "worktree", "remove", "--force", path)
	remove.Dir = repo.Path
	if out, err := remove.CombinedOutput(); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return fmt.Errorf("git worktree remove: %v\n%s", err, strings.TrimSpace(string(out)))
		}
	}
	return os.RemoveAll(path)
}

func startReview(repo *Repo, prNumber int) (string, error) {
	if info, err := os.Stat(repo.Path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("repo path missing: %s", repo.Path)
	}

	path := reviewPath(repo, prNumber)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return path, nil
	}

	worktree := exec.Command("git", "worktree", "add", "--detach", path)
	worktree.Dir = repo.Path
	if out, err := worktree.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %v\n%s", err, strings.TrimSpace(string(out)))
	}

	checkout := exec.Command("gh", "pr", "checkout", strconv.Itoa(prNumber))
	checkout.Dir = path
	if out, err := checkout.CombinedOutput(); err != nil {
		return path, fmt.Errorf("gh pr checkout: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
}
