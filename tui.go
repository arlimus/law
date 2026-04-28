package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type screen int

const (
	screenRepos screen = iota
	screenPRs
	screenActions
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

type runningAction struct {
	Name    string
	Command string
	Lines   []string
	Done    bool
	Stopped bool
	Code    int
	Err     error
	pipe    chan ActionEvent
	cmd     *exec.Cmd
}

type actionStartedMsg struct {
	action Action
	pr     int
	index  int
	pipe   chan ActionEvent
	cmd    *exec.Cmd
	err    error
}

type actionEventMsg struct {
	pipe chan ActionEvent
	evt  ActionEvent
}

type promptKind int

const (
	promptNone promptKind = iota
	promptNewName
	promptNewDesc
	promptEditDesc
)

type previewsLoadedMsg struct {
	prNumber int
	previews []vercelPreview
}

type actionGeneratedMsg struct {
	forEdit bool
	index   int
	name    string
	command string
	err     error
}

type model struct {
	cfg     *Config
	cfgPath string
	screen  screen

	reposCursor int

	currentRepo *Repo
	autoPR      int
	prs           []pullRequest
	prsCursor     int
	prsLoading    bool
	prsErr        string
	confirmDelete int

	actions       []Action
	actionsCursor int
	actionsErr    string
	actionsPR     int

	previews map[int][]vercelPreview

	running       map[int]map[int]*runningAction
	runningExpand bool

	prompt      promptKind
	input       string
	pendingName string
	editIndex   int
	generating  bool
	genErr      string
	inspecting  bool

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

func runActionCmd(action Action, pr, index int, dir string) tea.Cmd {
	return func() tea.Msg {
		ch, cmd, err := startAction(action, dir)
		if err != nil {
			return actionStartedMsg{action: action, pr: pr, index: index, err: err}
		}
		return actionStartedMsg{action: action, pr: pr, index: index, pipe: ch, cmd: cmd}
	}
}

func generateActionCmd(repoPath, name, userPrompt string) tea.Cmd {
	return func() tea.Msg {
		c, err := generateActionCommand(repoPath, userPrompt)
		return actionGeneratedMsg{name: name, command: c, err: err}
	}
}

func editActionCmdGen(repoPath, name, current, userPrompt string, index int) tea.Cmd {
	return func() tea.Msg {
		c, err := editActionCommand(repoPath, name, current, userPrompt)
		return actionGeneratedMsg{forEdit: true, index: index, name: name, command: c, err: err}
	}
}

func fetchPreviewsCmd(owner, name string, prNumber int) tea.Cmd {
	return func() tea.Msg {
		return previewsLoadedMsg{prNumber: prNumber, previews: fetchVercelPreviews(owner, name, prNumber)}
	}
}

func waitEventCmd(pipe chan ActionEvent) tea.Cmd {
	return func() tea.Msg {
		evt, ok := <-pipe
		if !ok {
			return actionEventMsg{pipe: pipe, evt: ActionEvent{Done: true}}
		}
		return actionEventMsg{pipe: pipe, evt: evt}
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
		if m.status == "refreshing..." {
			m.status = ""
		}
		return m, nil

	case reviewStartedMsg:
		if m.currentRepo == nil || m.currentRepo.URL != msg.repoURL {
			return m, nil
		}
		if msg.err != nil {
			m.status = "review error: " + msg.err.Error()
			return m, nil
		}
		m.status = "review ready at " + msg.path
		for i := range m.prs {
			if m.prs[i].Number == msg.prNumber {
				m.prs[i].InReview = true
			}
		}
		return m.openActions(msg.prNumber)

	case actionStartedMsg:
		if m.running == nil {
			m.running = map[int]map[int]*runningAction{}
		}
		if m.running[msg.pr] == nil {
			m.running[msg.pr] = map[int]*runningAction{}
		}
		if msg.err != nil {
			m.running[msg.pr][msg.index] = &runningAction{
				Name: msg.action.Name, Command: msg.action.Command,
				Done: true, Err: msg.err, Code: -1,
			}
			return m, nil
		}
		m.running[msg.pr][msg.index] = &runningAction{
			Name: msg.action.Name, Command: msg.action.Command,
			pipe: msg.pipe, cmd: msg.cmd,
		}
		return m, waitEventCmd(msg.pipe)

	case actionEventMsg:
		r := m.findRunningByPipe(msg.pipe)
		if r == nil {
			return m, nil
		}
		if msg.evt.Line != "" {
			r.Lines = append(r.Lines, msg.evt.Line)
			if len(r.Lines) > 5000 {
				r.Lines = r.Lines[len(r.Lines)-5000:]
			}
		}
		if msg.evt.Done {
			r.Done = true
			r.Code = msg.evt.Code
			r.Err = msg.evt.Err
			return m, nil
		}
		return m, waitEventCmd(msg.pipe)

	case previewsLoadedMsg:
		if m.previews == nil {
			m.previews = map[int][]vercelPreview{}
		}
		m.previews[msg.prNumber] = msg.previews
		return m, nil

	case actionGeneratedMsg:
		m.generating = false
		if msg.err != nil {
			m.genErr = msg.err.Error()
			return m, nil
		}
		if msg.command == "" {
			m.genErr = "claude returned an empty command"
			return m, nil
		}
		cfg, err := loadRepoConfig(m.currentRepo.Path)
		if err != nil {
			m.genErr = "load: " + err.Error()
			return m, nil
		}
		if msg.forEdit {
			if msg.index < 0 || msg.index >= len(cfg.Actions) {
				m.genErr = "action index out of range"
				return m, nil
			}
			cfg.Actions[msg.index].Command = msg.command
		} else {
			cfg.Actions = append(cfg.Actions, Action{Name: msg.name, Command: msg.command})
		}
		if err := saveRepoConfig(m.currentRepo.Path, cfg); err != nil {
			m.genErr = "save: " + err.Error()
			return m, nil
		}
		m.actions = cfg.Actions
		m.genErr = ""
		return m, nil

	case tea.KeyPressMsg:
		key := msg.Key()
		if key.Code == 'c' && key.Mod.Contains(tea.ModCtrl) {
			return m.quit()
		}
		switch m.screen {
		case screenRepos:
			return m.updateRepos(key)
		case screenPRs:
			return m.updatePRs(key)
		case screenActions:
			return m.updateActions(key)
		}
	}
	return m, nil
}

func (m model) quit() (tea.Model, tea.Cmd) {
	m.killAllRunning()
	return m, tea.Quit
}

func (m *model) killAllRunning() {
	for _, prMap := range m.running {
		for _, r := range prMap {
			if r != nil && !r.Done {
				r.Stopped = true
				killAction(r.cmd)
			}
		}
	}
}

func (m *model) findRunningByPipe(p chan ActionEvent) *runningAction {
	for _, prMap := range m.running {
		for _, r := range prMap {
			if r != nil && r.pipe == p {
				return r
			}
		}
	}
	return nil
}

func (m model) prHasRunning(prNumber int) bool {
	for _, r := range m.running[prNumber] {
		if r != nil && !r.Done {
			return true
		}
	}
	return false
}

func (m *model) stopPR(prNumber int) {
	for _, r := range m.running[prNumber] {
		if r != nil && !r.Done {
			r.Stopped = true
			killAction(r.cmd)
		}
	}
}

func (m model) updateRepos(key tea.Key) (tea.Model, tea.Cmd) {
	switch key.Code {
	case tea.KeyEscape, 'q':
		return m.quit()
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
	if key.Code != 'd' && m.confirmDelete != 0 {
		m.confirmDelete = 0
		if strings.HasPrefix(m.status, "press d again") {
			m.status = ""
		}
	}
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
		return m.quit()
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
			if pr.InReview {
				return m.openActions(pr.Number)
			}
			m.status = fmt.Sprintf("starting review for #%d...", pr.Number)
			return m, startReviewCmd(m.currentRepo, pr.Number)
		}
	case 'r':
		if m.currentRepo != nil && !m.prsLoading {
			m.prsLoading = true
			m.prsErr = ""
			m.status = "refreshing..."
			return m, fetchPRsCmd(m.currentRepo)
		}
	case 's':
		if m.prsCursor < len(m.prs) {
			m.stopPR(m.prs[m.prsCursor].Number)
		}
		return m, nil
	case 'd':
		if m.prsCursor >= len(m.prs) || !m.prs[m.prsCursor].InReview {
			return m, nil
		}
		pr := m.prs[m.prsCursor]
		if m.confirmDelete != pr.Number {
			m.confirmDelete = pr.Number
			m.status = fmt.Sprintf("press d again to delete .review%d", pr.Number)
			return m, nil
		}
		m.confirmDelete = 0
		m.stopPR(pr.Number)
		if err := removeReview(m.currentRepo, pr.Number); err != nil {
			m.status = "delete failed: " + err.Error()
			return m, nil
		}
		delete(m.running, pr.Number)
		if pr.Status == "open" {
			m.prs[m.prsCursor].InReview = false
		} else {
			m.prs = append(m.prs[:m.prsCursor], m.prs[m.prsCursor+1:]...)
			if m.prsCursor >= len(m.prs) && m.prsCursor > 0 {
				m.prsCursor--
			}
		}
		m.status = fmt.Sprintf("deleted .review%d", pr.Number)
		return m, nil
	}
	return m, nil
}

func (m model) openActions(prNumber int) (tea.Model, tea.Cmd) {
	m.screen = screenActions
	m.actionsPR = prNumber
	m.actionsCursor = 0
	cfg, err := loadRepoConfig(m.currentRepo.Path)
	if err != nil {
		m.actionsErr = err.Error()
		m.actions = nil
	} else {
		m.actions = cfg.Actions
		m.actionsErr = ""
	}
	var cmd tea.Cmd
	if _, cached := m.previews[prNumber]; !cached {
		if owner, repo, ok := m.currentRepo.ownerRepo(); ok {
			cmd = fetchPreviewsCmd(owner, repo, prNumber)
		}
	}
	return m, cmd
}

func (m model) updateActions(key tea.Key) (tea.Model, tea.Cmd) {
	if m.prompt != promptNone {
		return m.updatePrompt(key)
	}
	if m.generating {
		return m, nil
	}
	if key.Code == 'o' && key.Mod.Contains(tea.ModCtrl) {
		m.runningExpand = !m.runningExpand
		return m, nil
	}
	if key.Mod.Contains(tea.ModCtrl) && key.Mod.Contains(tea.ModShift) {
		switch key.Code {
		case tea.KeyUp:
			return m.moveAction(-1)
		case tea.KeyDown:
			return m.moveAction(1)
		}
	}
	switch key.Code {
	case tea.KeyEscape:
		m.runningExpand = false
		m.screen = screenPRs
		m.actions = nil
		m.actionsCursor = 0
		m.actionsErr = ""
		m.actionsPR = 0
		m.genErr = ""
		m.inspecting = false
		return m, nil
	case 'q':
		return m.quit()
	case tea.KeyUp, 'k':
		if m.actionsCursor > 0 {
			m.actionsCursor--
		}
	case tea.KeyDown, 'j':
		if m.actionsCursor < len(m.actions)-1 {
			m.actionsCursor++
		}
	case 'n':
		m.prompt = promptNewName
		m.input = ""
		m.genErr = ""
		return m, nil
	case 'e':
		if m.actionsCursor < len(m.actions) {
			m.prompt = promptEditDesc
			m.editIndex = m.actionsCursor
			m.input = ""
			m.genErr = ""
		}
		return m, nil
	case 'i':
		if m.actionsCursor < len(m.actions) {
			m.inspecting = !m.inspecting
		}
		return m, nil
	case tea.KeyEnter:
		if m.actionsCursor >= len(m.actions) {
			return m, nil
		}
		if r, ok := m.running[m.actionsPR][m.actionsCursor]; ok && !r.Done {
			return m, nil
		}
		action := m.actions[m.actionsCursor]
		m.runningExpand = false
		dir := reviewPath(m.currentRepo, m.actionsPR)
		return m, runActionCmd(action, m.actionsPR, m.actionsCursor, dir)
	case 's':
		if r, ok := m.running[m.actionsPR][m.actionsCursor]; ok && !r.Done {
			r.Stopped = true
			killAction(r.cmd)
		}
		return m, nil
	}
	return m, nil
}

func (m model) moveAction(delta int) (tea.Model, tea.Cmd) {
	i := m.actionsCursor
	j := i + delta
	if i < 0 || i >= len(m.actions) || j < 0 || j >= len(m.actions) {
		return m, nil
	}
	m.actions[i], m.actions[j] = m.actions[j], m.actions[i]
	if prMap := m.running[m.actionsPR]; prMap != nil {
		ri, hasI := prMap[i]
		rj, hasJ := prMap[j]
		delete(prMap, i)
		delete(prMap, j)
		if hasI {
			prMap[j] = ri
		}
		if hasJ {
			prMap[i] = rj
		}
	}
	m.actionsCursor = j
	if err := saveRepoConfig(m.currentRepo.Path, &RepoConfig{Actions: m.actions}); err != nil {
		m.genErr = "save: " + err.Error()
	}
	return m, nil
}

func (m model) updatePrompt(key tea.Key) (tea.Model, tea.Cmd) {
	switch key.Code {
	case tea.KeyEscape:
		m.prompt = promptNone
		m.input = ""
		m.pendingName = ""
		m.editIndex = 0
		return m, nil
	case tea.KeyEnter:
		v := strings.TrimSpace(m.input)
		if v == "" {
			return m, nil
		}
		return m.submitPrompt(v)
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
		return m, nil
	}
	if key.Text != "" && key.Mod&^tea.ModShift == 0 {
		m.input += key.Text
	}
	return m, nil
}

func (m model) submitPrompt(v string) (tea.Model, tea.Cmd) {
	switch m.prompt {
	case promptNewName:
		m.pendingName = v
		m.input = ""
		m.prompt = promptNewDesc
		return m, nil
	case promptNewDesc:
		name := m.pendingName
		m.pendingName = ""
		m.input = ""
		m.prompt = promptNone
		m.generating = true
		m.genErr = ""
		return m, generateActionCmd(m.currentRepo.Path, name, v)
	case promptEditDesc:
		if m.editIndex < 0 || m.editIndex >= len(m.actions) {
			m.prompt = promptNone
			m.input = ""
			return m, nil
		}
		action := m.actions[m.editIndex]
		idx := m.editIndex
		m.editIndex = 0
		m.input = ""
		m.prompt = promptNone
		m.generating = true
		m.genErr = ""
		return m, editActionCmdGen(m.currentRepo.Path, action.Name, action.Command, v, idx)
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
	authorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	mergedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
)

// hyperlink wraps text in an OSC 8 escape so kitty/iTerm2/wezterm/alacritty/ghostty
// render it as a clickable link. Terminals without OSC 8 support just show the text.
func hyperlink(url, text string) string {
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// Nerd Fonts Octicons for PR state.
const (
	mergedIcon = "" // nf-oct-git_merge
	closedIcon = "" // nf-oct-git_pull_request_closed
)

// githubIcon is the Nerd Fonts FontAwesome github glyph (nf-fa-github, U+F09B).
const githubIcon = ""

func (m model) View() tea.View {
	var v tea.View
	switch m.screen {
	case screenPRs:
		v = tea.NewView(m.renderPRs())
	case screenActions:
		v = tea.NewView(m.renderActions())
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
			mark := "  "
			if pr.InReview {
				if m.prHasRunning(pr.Number) {
					mark = markStyle.Render("*") + flashStyle.Render("●")
				} else {
					mark = markStyle.Render("*") + " "
				}
			}
			numStr := fmt.Sprintf("%*d", numWidth, pr.Number)
			ago := dimStyle.Render(fmt.Sprintf("%*s", agoWidth, agos[i]))
			statusIcon := "  "
			switch pr.Status {
			case "merged":
				statusIcon = mergedStyle.Render(mergedIcon) + " "
			case "closed", "unknown":
				statusIcon = warnStyle.Render(closedIcon) + " "
			}
			rest := ": " + pr.Title
			if i == m.prsCursor {
				cursor = cursorStyle.Render("▸ ")
				numStr = selectedStyle.Render(numStr)
				rest = selectedStyle.Render(rest)
			}
			if owner, repo, ok := m.currentRepo.ownerRepo(); ok {
				numStr = hyperlink(fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, pr.Number), numStr)
			}
			line := cursor + mark + " " + ago + "  " + statusIcon + numStr + rest
			if pr.Author != "" {
				line += "  " + authorStyle.Render("@"+pr.Author)
			}
			b.WriteString(line + "\n")
		}
	}

	if m.status != "" {
		b.WriteString("\n" + flashStyle.Render(m.status) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render("↑/↓: navigate • enter: start review • r: refresh • s: stop running • d: delete folder • esc: back • q: quit") + "\n")
	return b.String()
}

func (m model) renderActions() string {
	var b strings.Builder
	if m.currentRepo != nil {
		if owner, repo, ok := m.currentRepo.ownerRepo(); ok {
			b.WriteString(titleStyle.Render(fmt.Sprintf("law - %s %s/%s #%d", githubIcon, owner, repo, m.actionsPR)) + "\n")
			b.WriteString(dimStyle.Render(fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, m.actionsPR)) + "\n")
		} else {
			b.WriteString(titleStyle.Render(fmt.Sprintf("law - %s %s #%d", githubIcon, m.currentRepo.URL, m.actionsPR)) + "\n")
		}
		for _, p := range m.previews[m.actionsPR] {
			b.WriteString(authorStyle.Render(previewIcon) + " " + p.Name + ": " + dimStyle.Render(p.URL) + "\n")
		}
		b.WriteString("\n")
	} else {
		b.WriteString(titleStyle.Render("law - actions") + "\n\n")
	}

	switch {
	case m.actionsErr != "":
		b.WriteString(warnStyle.Render("  "+m.actionsErr) + "\n")
	case len(m.actions) == 0:
		b.WriteString(dimStyle.Render("  no actions configured") + "\n")
		if m.currentRepo != nil {
			b.WriteString(dimStyle.Render("  define them in "+repoConfigPath(m.currentRepo.Path)) + "\n")
		}
	default:
		for i, a := range m.actions {
			cursor := "  "
			text := a.Name
			if i == m.actionsCursor {
				cursor = cursorStyle.Render("▸ ")
				text = selectedStyle.Render(text)
			}
			b.WriteString(cursor + runMarker(m.running[m.actionsPR][i]) + " " + text + "\n")
		}
	}

	if m.inspecting && m.actionsCursor < len(m.actions) {
		b.WriteString("\n" + renderInspectPanel(m.actions[m.actionsCursor]))
	}

	if r, ok := m.running[m.actionsPR][m.actionsCursor]; ok && r != nil {
		b.WriteString("\n" + renderRunningPanel(r, m.runningExpand))
	}

	if m.prompt != promptNone {
		b.WriteString("\n" + renderPrompt(m))
	} else if m.generating {
		b.WriteString("\n" + flashStyle.Render("generating with claude...") + "\n")
	}
	if m.genErr != "" {
		b.WriteString("\n" + warnStyle.Render(m.genErr) + "\n")
	}

	inspectHint := "i: inspect"
	if m.inspecting {
		inspectHint = "i: hide"
	}
	var help string
	switch {
	case m.prompt != promptNone:
		help = "enter: submit • esc: cancel"
	case m.generating:
		help = "waiting for claude..."
	default:
		base := "↑/↓: navigate • enter: run • s: stop • n: new • e: edit • " + inspectHint
		if r, ok := m.running[m.actionsPR][m.actionsCursor]; ok && r != nil {
			base += " • ctrl+o: expand"
		}
		help = base + " • esc: back • q: quit"
	}
	b.WriteString("\n" + dimStyle.Render(help) + "\n")
	return b.String()
}

func renderInspectPanel(a Action) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", flashStyle.Render("▶ inspecting"), a.Name)
	if a.Command == "" {
		b.WriteString(dimStyle.Render("│ (empty)") + "\n")
		return b.String()
	}
	for line := range strings.SplitSeq(a.Command, "\n") {
		b.WriteString(dimStyle.Render("│ ") + line + "\n")
	}
	return b.String()
}

func renderPrompt(m model) string {
	var label string
	switch m.prompt {
	case promptNewName:
		label = "name for the new action:"
	case promptNewDesc:
		label = fmt.Sprintf("describe %q (claude will generate the shell command):", m.pendingName)
	case promptEditDesc:
		if m.editIndex < len(m.actions) {
			a := m.actions[m.editIndex]
			label = fmt.Sprintf("modify %q (current: %s):", a.Name, a.Command)
		} else {
			label = "modify:"
		}
	}
	return dimStyle.Render(label) + "\n" + "› " + m.input + cursorStyle.Render("█") + "\n"
}

func runMarker(r *runningAction) string {
	if r == nil {
		return " "
	}
	switch {
	case !r.Done:
		return flashStyle.Render("▶")
	case r.Stopped:
		return warnStyle.Render("■")
	case r.Err != nil || r.Code != 0:
		return warnStyle.Render("✗")
	default:
		return selectedStyle.Render("✓")
	}
}

func renderRunningPanel(r *runningAction, expand bool) string {
	var b strings.Builder

	var status string
	switch {
	case r.Stopped && !r.Done:
		status = warnStyle.Render("■ stopping")
	case r.Stopped:
		status = warnStyle.Render("■ stopped")
	case !r.Done:
		status = flashStyle.Render("▶ running")
	case r.Err != nil:
		status = warnStyle.Render("✗ " + r.Err.Error())
	case r.Code != 0:
		status = warnStyle.Render(fmt.Sprintf("✗ exit %d", r.Code))
	default:
		status = selectedStyle.Render("✓ done")
	}
	fmt.Fprintf(&b, "%s  %s\n", status, r.Name)
	b.WriteString(dimStyle.Render("$ "+r.Command) + "\n")

	lines := r.Lines
	const tailN = 3
	hidden := 0
	if !expand && len(lines) > tailN {
		hidden = len(lines) - tailN
		lines = lines[hidden:]
	}
	if !expand && hidden > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("│ … %d earlier lines (ctrl+o to expand)", hidden)) + "\n")
	}
	for _, ln := range lines {
		b.WriteString(dimStyle.Render("│ ") + ln + "\n")
	}
	return b.String()
}
