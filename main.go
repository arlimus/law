package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
)

func main() {
	if len(os.Args) > 2 {
		fmt.Fprintf(os.Stderr, "usage: %s [github-pr-url]\n", os.Args[0])
		os.Exit(2)
	}

	cfg, cfgPath, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	var flash string
	var autoRepo *Repo
	if repo, ok := detectLocalRepo(); ok {
		if !cfg.hasRepoURL(repo.URL) {
			cfg.Repos = append(cfg.Repos, repo)
			if err := saveConfig(cfg, cfgPath); err != nil {
				fmt.Fprintf(os.Stderr, "save config: %v\n", err)
				os.Exit(1)
			}
			flash = "added repo: " + repo.URL
		}
		// Skip repo selection: jump straight to the repo we're running in.
		autoRepo = cfg.findRepoByURL(repo.URL)
	}

	var autoPR int
	if len(os.Args) == 2 {
		pr, err := parsePR(os.Args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse PR: %v\n", err)
			os.Exit(1)
		}
		repo := cfg.findRepoByURL(pr.repoURL())
		if repo == nil {
			fmt.Fprintf(os.Stderr, "no tracked repo matches %s\nrun law in the repo root first\n", pr.repoURL())
			os.Exit(1)
		}
		autoRepo = repo
		autoPR = pr.number
	}

	m := newModel(cfg, cfgPath, flash, autoRepo, autoPR)
	if _, err := tea.NewProgram(m).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
