package main

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type screen int

const (
	screenRepos screen = iota
	screenPRs
)

type prsLoadedMsg struct {
	repoURL string
	prs     []pullRequest
	err     error
}

type reviewStartedMsg struct {
	repoURL  string
	prNumber int
	path     string
	err      error
}

type model struct {
	cfg     *Config
	cfgPath string
	screen  screen

	reposCursor int

	currentRepo *Repo
	autoPR      int
	prs         []pullRequest
	prsCursor   int
	prsLoading  bool
	prsErr      string

	status string
	flash  string

	width, height int
}

func newModel(cfg *Config, cfgPath, flash string, autoRepo *Repo, autoPR int) model {
	m := model{
		cfg:     cfg,
		cfgPath: cfgPath,
		flash:   flash,
	}
	if autoRepo != nil {
		m.screen = screenPRs
		m.currentRepo = autoRepo
		m.autoPR = autoPR
		m.prsLoading = true
		if autoPR > 0 {
			m.status = fmt.Sprintf("starting review for #%d...", autoPR)
		}
	}
	return m
}

func (m model) Init() tea.Cmd {
	if m.screen == screenPRs && m.currentRepo != nil {
		cmds := []tea.Cmd{fetchPRsCmd(m.currentRepo)}
		if m.autoPR > 0 {
			cmds = append(cmds, startReviewCmd(m.currentRepo, m.autoPR))
		}
		return tea.Batch(cmds...)
	}
	return nil
}

func fetchPRsCmd(repo *Repo) tea.Cmd {
	repoURL := repo.URL
	return func() tea.Msg {
		prs, err := fetchPRs(repo)
		return prsLoadedMsg{repoURL: repoURL, prs: prs, err: err}
	}
}

func startReviewCmd(repo *Repo, prNumber int) tea.Cmd {
	repoURL := repo.URL
	return func() tea.Msg {
		path, err := startReview(repo, prNumber)
		return reviewStartedMsg{repoURL: repoURL, prNumber: prNumber, path: path, err: err}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case prsLoadedMsg:
		if m.currentRepo == nil || m.currentRepo.URL != msg.repoURL {
			return m, nil
		}
		m.prsLoading = false
		if msg.err != nil {
			m.prsErr = msg.err.Error()
			m.prs = nil
		} else {
			m.prs = msg.prs
			m.prsErr = ""
			if m.prsCursor >= len(m.prs) {
				m.prsCursor = 0
			}
		}
		return m, nil

	case reviewStartedMsg:
		if m.currentRepo == nil || m.currentRepo.URL != msg.repoURL {
			return m, nil
		}
		if msg.err != nil {
			m.status = "review error: " + msg.err.Error()
		} else {
			m.status = "review ready at " + msg.path
			for i := range m.prs {
				if m.prs[i].Number == msg.prNumber {
					m.prs[i].InReview = true
				}
			}
		}
		return m, nil

	case tea.KeyPressMsg:
		key := msg.Key()
		if key.Code == 'c' && key.Mod.Contains(tea.ModCtrl) {
			return m, tea.Quit
		}
		switch m.screen {
		case screenRepos:
			return m.updateRepos(key)
		case screenPRs:
			return m.updatePRs(key)
		}
	}
	return m, nil
}

func (m model) updateRepos(key tea.Key) (tea.Model, tea.Cmd) {
	switch key.Code {
	case tea.KeyEscape, 'q':
		return m, tea.Quit
	case tea.KeyUp, 'k':
		if m.reposCursor > 0 {
			m.reposCursor--
		}
	case tea.KeyDown, 'j':
		if m.reposCursor < len(m.cfg.Repos)-1 {
			m.reposCursor++
		}
	case tea.KeyEnter:
		if m.reposCursor < len(m.cfg.Repos) {
			m.currentRepo = &m.cfg.Repos[m.reposCursor]
			m.screen = screenPRs
			m.prs = nil
			m.prsCursor = 0
			m.prsErr = ""
			m.prsLoading = true
			m.status = ""
			return m, fetchPRsCmd(m.currentRepo)
		}
	}
	return m, nil
}

func (m model) updatePRs(key tea.Key) (tea.Model, tea.Cmd) {
	switch key.Code {
	case tea.KeyEscape:
		m.screen = screenRepos
		m.currentRepo = nil
		m.prs = nil
		m.prsCursor = 0
		m.prsErr = ""
		m.prsLoading = false
		m.status = ""
		return m, nil
	case 'q':
		return m, tea.Quit
	case tea.KeyUp, 'k':
		if m.prsCursor > 0 {
			m.prsCursor--
		}
	case tea.KeyDown, 'j':
		if m.prsCursor < len(m.prs)-1 {
			m.prsCursor++
		}
	case tea.KeyEnter:
		if m.currentRepo != nil && m.prsCursor < len(m.prs) {
			pr := m.prs[m.prsCursor]
			m.status = fmt.Sprintf("starting review for #%d...", pr.Number)
			return m, startReviewCmd(m.currentRepo, pr.Number)
		}
	}
	return m, nil
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	flashStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	markStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
)

func (m model) View() tea.View {
	var v tea.View
	switch m.screen {
	case screenPRs:
		v = tea.NewView(m.renderPRs())
	default:
		v = tea.NewView(m.renderRepos())
	}
	v.AltScreen = true
	return v
}

func (m model) renderRepos() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("law — repositories") + "\n\n")

	if len(m.cfg.Repos) == 0 {
		b.WriteString(dimStyle.Render("  no repos yet — cd into a repo's root and run law to add it") + "\n")
	}

	for i, r := range m.cfg.Repos {
		count := len(inProgressReviews(r.Path))
		cursor := "  "
		urlStr := r.URL
		if i == m.reposCursor {
			cursor = cursorStyle.Render("▸ ")
			urlStr = selectedStyle.Render(r.URL)
		}
		meta := dimStyle.Render(fmt.Sprintf("  %s  (%d reviews)", r.Path, count))
		b.WriteString(cursor + urlStr + meta + "\n")
	}

	if m.flash != "" {
		b.WriteString("\n" + flashStyle.Render(m.flash) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render("↑/↓: navigate • enter: open PRs • q: quit") + "\n")
	return b.String()
}

func (m model) renderPRs() string {
	var b strings.Builder
	header := "law — PRs"
	if m.currentRepo != nil {
		header = "law — " + m.currentRepo.URL
	}
	b.WriteString(titleStyle.Render(header) + "\n\n")

	switch {
	case m.prsLoading && len(m.prs) == 0 && m.prsErr == "":
		b.WriteString(dimStyle.Render("  loading...") + "\n")
	case m.prsErr != "":
		b.WriteString(warnStyle.Render("  "+m.prsErr) + "\n")
	case len(m.prs) == 0:
		b.WriteString(dimStyle.Render("  no open PRs") + "\n")
	default:
		numWidth, agoWidth := 1, 0
		agos := make([]string, len(m.prs))
		for i, pr := range m.prs {
			if w := len(strconv.Itoa(pr.Number)); w > numWidth {
				numWidth = w
			}
			agos[i] = humanizeAgo(pr.CreatedAt)
			if len(agos[i]) > agoWidth {
				agoWidth = len(agos[i])
			}
		}
		for i, pr := range m.prs {
			cursor := "  "
			mark := " "
			if pr.InReview {
				mark = markStyle.Render("*")
			}
			numStr := fmt.Sprintf("%*d", numWidth, pr.Number)
			ago := dimStyle.Render(fmt.Sprintf("%*s", agoWidth, agos[i]))
			text := fmt.Sprintf("%s: %s", numStr, pr.Title)
			if i == m.prsCursor {
				cursor = cursorStyle.Render("▸ ")
				text = selectedStyle.Render(text)
			}
			b.WriteString(cursor + mark + " " + ago + "  " + text + "\n")
		}
	}

	if m.status != "" {
		b.WriteString("\n" + flashStyle.Render(m.status) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render("↑/↓: navigate • enter: start review • esc: back • q: quit") + "\n")
	return b.String()
}
