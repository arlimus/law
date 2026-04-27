package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type Action struct {
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
}

type RepoConfig struct {
	Actions []Action `json:"actions"`
}

const repoConfigName = ".law.json"

func repoConfigPath(repoPath string) string {
	return filepath.Join(repoPath, repoConfigName)
}

func loadRepoConfig(repoPath string) (*RepoConfig, error) {
	data, err := os.ReadFile(repoConfigPath(repoPath))
	if err != nil {
		if os.IsNotExist(err) {
			return &RepoConfig{}, nil
		}
		return nil, err
	}
	cfg := &RepoConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func saveRepoConfig(repoPath string, cfg *RepoConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(repoConfigPath(repoPath), append(data, '\n'), 0o644)
}

// generateActionCommand asks the claude CLI to produce a shell command for the
// user's request. The command runs in repoPath so claude has the repo as context.
func generateActionCommand(repoPath, userPrompt string) (string, error) {
	prompt := fmt.Sprintf(`You generate a single shell command for a code-review helper. The command runs via "sh -c" in this repo.

User request: %s

Output ONLY the shell command. No explanation, no markdown code fences, no leading "$ ". Multiline is OK if the user truly needs it.`, userPrompt)
	return runClaude(repoPath, prompt)
}

func editActionCommand(repoPath, name, current, userPrompt string) (string, error) {
	prompt := fmt.Sprintf(`Modify an existing shell command for a code-review helper. The command runs via "sh -c" in this repo.

Action name: %s
Current command:
%s

User wants to modify it as follows: %s

Output ONLY the new shell command. No explanation, no markdown code fences, no leading "$ ".`, name, current, userPrompt)
	return runClaude(repoPath, prompt)
}

func runClaude(dir, prompt string) (string, error) {
	cmd := exec.Command("claude", "-p", prompt)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("claude failed: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("running claude: %w", err)
	}
	return cleanCommand(string(out)), nil
}

func cleanCommand(s string) string {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "```"); ok {
		if i := strings.Index(rest, "\n"); i >= 0 {
			rest = rest[i+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(rest, "```"))
	}
	return s
}

type ActionEvent struct {
	Line string
	Done bool
	Err  error
	Code int
}

// startAction runs the action command via "sh -c" in dir, streaming stdout/stderr
// lines on the returned channel. A final ActionEvent with Done=true is always sent
// before the channel is closed. Caller can kill the returned *exec.Cmd's Process.
func startAction(action Action, dir string) (chan ActionEvent, *exec.Cmd, error) {
	cmd := exec.Command("sh", "-c", action.Command)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	ch := make(chan ActionEvent, 64)
	go func() {
		defer close(ch)

		var wg sync.WaitGroup
		wg.Add(2)
		stream := func(r io.Reader) {
			defer wg.Done()
			s := bufio.NewScanner(r)
			s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for s.Scan() {
				ch <- ActionEvent{Line: s.Text()}
			}
		}
		go stream(stdout)
		go stream(stderr)
		wg.Wait()

		code := 0
		if werr := cmd.Wait(); werr != nil {
			if ee, ok := werr.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				ch <- ActionEvent{Err: werr, Done: true, Code: -1}
				return
			}
		}
		ch <- ActionEvent{Done: true, Code: code}
	}()
	return ch, cmd, nil
}
