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
	Code    int
	Err     error
	pipe    chan ActionEvent
	cmd     *exec.Cmd
}

type actionStartedMsg struct {
	action Action
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
	prs         []pullRequest
	prsCursor   int
	prsLoading  bool
	prsErr      string

	actions       []Action
	actionsCursor int
	actionsErr    string
	actionsPR     int

	running       *runningAction
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

func runActionCmd(action Action, dir string) tea.Cmd {
	return func() tea.Msg {
		ch, cmd, err := startAction(action, dir)
		if err != nil {
			return actionStartedMsg{action: action, err: err}
		}
		return actionStartedMsg{action: action, pipe: ch, cmd: cmd}
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
		if msg.err != nil {
			m.running = &runningAction{
				Name: msg.action.Name, Command: msg.action.Command,
				Done: true, Err: msg.err, Code: -1,
			}
			return m, nil
		}
		m.running = &runningAction{
			Name: msg.action.Name, Command: msg.action.Command,
			pipe: msg.pipe, cmd: msg.cmd,
		}
		return m, waitEventCmd(msg.pipe)

	case actionEventMsg:
		if m.running == nil || m.running.pipe != msg.pipe {
			return m, nil
		}
		if msg.evt.Line != "" {
			m.running.Lines = append(m.running.Lines, msg.evt.Line)
			if len(m.running.Lines) > 5000 {
				m.running.Lines = m.running.Lines[len(m.running.Lines)-5000:]
			}
		}
		if msg.evt.Done {
			m.running.Done = true
			m.running.Code = msg.evt.Code
			m.running.Err = msg.evt.Err
			return m, nil
		}
		return m, waitEventCmd(msg.pipe)

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
			return m, tea.Quit
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
			if pr.InReview {
				return m.openActions(pr.Number)
			}
			m.status = fmt.Sprintf("starting review for #%d...", pr.Number)
			return m, startReviewCmd(m.currentRepo, pr.Number)
		}
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
	return m, nil
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
	switch key.Code {
	case tea.KeyEscape:
		if m.running != nil && !m.running.Done && m.running.cmd != nil && m.running.cmd.Process != nil {
			_ = m.running.cmd.Process.Kill()
		}
		m.running = nil
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
		return m, tea.Quit
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
		if m.running != nil && !m.running.Done {
			return m, nil
		}
		action := m.actions[m.actionsCursor]
		m.runningExpand = false
		dir := reviewPath(m.currentRepo, m.actionsPR)
		return m, runActionCmd(action, dir)
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
)

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
			line := cursor + mark + " " + ago + "  " + text
			if pr.Author != "" {
				line += "  " + authorStyle.Render("@"+pr.Author)
			}
			b.WriteString(line + "\n")
		}
	}

	if m.status != "" {
		b.WriteString("\n" + flashStyle.Render(m.status) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render("↑/↓: navigate • enter: start review • esc: back • q: quit") + "\n")
	return b.String()
}

func (m model) renderActions() string {
	var b strings.Builder
	header := "law — actions"
	if m.currentRepo != nil {
		header = fmt.Sprintf("law — %s#%d actions", m.currentRepo.URL, m.actionsPR)
	}
	b.WriteString(titleStyle.Render(header) + "\n\n")

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
			b.WriteString(cursor + text + "\n")
		}
	}

	if m.inspecting && m.actionsCursor < len(m.actions) {
		b.WriteString("\n" + renderInspectPanel(m.actions[m.actionsCursor]))
	}

	if m.running != nil {
		b.WriteString("\n" + renderRunningPanel(m.running, m.runningExpand))
	}

	if m.prompt != promptNone {
		b.WriteString("\n" + renderPrompt(m))
	} else if m.generating {
		b.WriteString("\n" + flashStyle.Render("generating with claude...") + "\n")
	}
	if m.genErr != "" {
		b.WriteString("\n" + warnStyle.Render(m.genErr) + "\n")
	}

	var help string
	switch {
	case m.prompt != promptNone:
		help = "enter: submit • esc: cancel"
	case m.generating:
		help = "waiting for claude..."
	case m.running != nil:
		help = "↑/↓: navigate • enter: run • n: new • e: edit • i: inspect • ctrl+o: expand • esc: back • q: quit"
	default:
		help = "↑/↓: navigate • enter: run • n: new • e: edit • i: inspect • esc: back • q: quit"
	}
	b.WriteString("\n" + dimStyle.Render(help) + "\n")
	return b.String()
}

func renderInspectPanel(a Action) string {
	var b strings.Builder
	b.WriteString(selectedStyle.Render(a.Name) + "\n")
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

func renderRunningPanel(r *runningAction, expand bool) string {
	var b strings.Builder

	var status string
	switch {
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
