package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

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
	ws     wsKey
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
	promptNewBranch
)

type branchStatusMsg struct {
	key      wsKey
	commits  int
	prNumber int // non-zero if a PR for the branch has appeared
	err      error
}

type branchTickMsg struct {
	key wsKey
}

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

// workspaceItem is a row in the workspaces view: either a pre-PR branch or a PR.
type workspaceItem struct {
	Branch string       // non-empty for branch row
	PR     *pullRequest // non-nil for PR row
	Path   string       // for branch rows, the worktree path
}

type model struct {
	cfg     *Config
	cfgPath string
	screen  screen

	reposCursor int

	currentRepo *Repo
	autoPR      int
	prs           []pullRequest
	branches      []workspace // pre-PR local branch workspaces (no matching PR)
	prsCursor     int
	prsLoading    bool
	prsErr        string
	confirmDelete wsKey

	actions       []Action
	actionsCursor int
	actionsErr    string
	activeWS      wsKey
	actionsPath   string

	branchCommits int // for branch mode: commits ahead of origin/main

	previews map[int][]vercelPreview

	running       map[wsKey]map[int]*runningAction
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

func runActionCmd(action Action, ws wsKey, index int, dir string) tea.Cmd {
	return func() tea.Msg {
		ch, cmd, err := startAction(action, dir)
		if err != nil {
			return actionStartedMsg{action: action, ws: ws, index: index, err: err}
		}
		return actionStartedMsg{action: action, ws: ws, index: index, pipe: ch, cmd: cmd}
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

func branchStatusCmd(branch, path string, prs []pullRequest) tea.Cmd {
	key := branchKey(branch)
	return func() tea.Msg {
		commits, err := branchCommitCount(path)
		msg := branchStatusMsg{key: key, commits: commits, err: err}
		for _, pr := range prs {
			if pr.HeadRef == branch {
				msg.prNumber = pr.Number
				break
			}
		}
		return msg
	}
}

func branchTickCmd(key wsKey) tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return branchTickMsg{key: key} })
}

// pushAndOpenPRCmd runs `git push -u origin <branch>` followed by `gh pr create`
// with title/body taken from the first commit ahead of origin/main. The whole
// flow streams through the existing runningAction machinery so its stdout is
// visible in the running panel.
func pushAndOpenPRCmd(ws wsKey, path, branch string) tea.Cmd {
	cmd := fmt.Sprintf(`set -e
git push -u origin %[1]s
SHA=$(git log origin/main..HEAD --reverse --format=%%H | head -1)
TITLE=$(git show -s --format=%%s "$SHA")
BODY=$(git show -s --format=%%b "$SHA")
gh pr create --title "$TITLE" --body "$BODY"
`, shellQuote(branch))
	action := Action{Name: "push and open PR", Command: cmd}
	return runActionCmd(action, ws, syntheticIndex, path)
}

// shellQuote wraps s in single quotes, escaping embedded single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
		}
		m.refreshBranches()
		if m.prsCursor >= len(m.visibleWorkspaces()) {
			m.prsCursor = 0
		}
		if m.status == "refreshing..." {
			m.status = ""
		}
		// If we're sitting on a branch workspace whose PR has just appeared,
		// transition to PR mode (rename worktree, rekey running map) now —
		// don't wait for the next 3s tick.
		if m.activeWS.isBranch() {
			for _, pr := range m.prs {
				if pr.HeadRef == m.activeWS.Branch {
					return m.transitionBranchToPR(m.activeWS.Branch, pr.Number)
				}
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
		return m.openActionsPR(msg.prNumber)

	case actionStartedMsg:
		if m.running == nil {
			m.running = map[wsKey]map[int]*runningAction{}
		}
		if m.running[msg.ws] == nil {
			m.running[msg.ws] = map[int]*runningAction{}
		}
		if msg.err != nil {
			m.running[msg.ws][msg.index] = &runningAction{
				Name: msg.action.Name, Command: msg.action.Command,
				Done: true, Err: msg.err, Code: -1,
			}
			return m, nil
		}
		m.running[msg.ws][msg.index] = &runningAction{
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
			// If the synthetic push-and-open-PR just completed successfully,
			// refresh the PR list immediately so the new PR shows up without
			// waiting for the next 3s tick.
			if m.activeWS.isBranch() && msg.evt.Err == nil && msg.evt.Code == 0 {
				if syn, ok := m.running[m.activeWS][syntheticIndex]; ok && syn == r {
					return m, fetchPRsCmd(m.currentRepo)
				}
			}
			return m, nil
		}
		return m, waitEventCmd(msg.pipe)

	case branchStatusMsg:
		if m.activeWS != msg.key {
			return m, nil
		}
		if msg.err == nil {
			m.branchCommits = msg.commits
		}
		if msg.prNumber != 0 {
			return m.transitionBranchToPR(msg.key.Branch, msg.prNumber)
		}
		return m, nil

	case branchTickMsg:
		if m.activeWS != msg.key {
			return m, nil
		}
		return m, tea.Batch(
			branchStatusCmd(msg.key.Branch, m.actionsPath, m.prs),
			branchTickCmd(msg.key),
		)

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
	for _, wsMap := range m.running {
		for _, r := range wsMap {
			if r != nil && !r.Done {
				r.Stopped = true
				killAction(r.cmd)
			}
		}
	}
}

func (m *model) findRunningByPipe(p chan ActionEvent) *runningAction {
	for _, wsMap := range m.running {
		for _, r := range wsMap {
			if r != nil && r.pipe == p {
				return r
			}
		}
	}
	return nil
}

func (m model) wsHasRunning(key wsKey) bool {
	for _, r := range m.running[key] {
		if r != nil && !r.Done {
			return true
		}
	}
	return false
}

func (m *model) stopWS(key wsKey) {
	for _, r := range m.running[key] {
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
	if key.Code != 'd' && !m.confirmDelete.isZero() {
		m.confirmDelete = wsKey{}
		if strings.HasPrefix(m.status, "press d again") {
			m.status = ""
		}
	}
	if m.prompt == promptNewBranch {
		return m.updatePrompt(key)
	}
	items := m.visibleWorkspaces()
	switch key.Code {
	case tea.KeyEscape:
		m.screen = screenRepos
		m.currentRepo = nil
		m.prs = nil
		m.branches = nil
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
		if m.prsCursor < len(items)-1 {
			m.prsCursor++
		}
	case tea.KeyEnter:
		if m.currentRepo == nil || m.prsCursor >= len(items) {
			return m, nil
		}
		it := items[m.prsCursor]
		if it.Branch != "" {
			return m.openActionsBranch(it.Branch, it.Path)
		}
		pr := it.PR
		if pr.InReview {
			return m.openActionsPR(pr.Number)
		}
		m.status = fmt.Sprintf("starting review for #%d...", pr.Number)
		return m, startReviewCmd(m.currentRepo, pr.Number)
	case 'n':
		if m.currentRepo == nil {
			return m, nil
		}
		m.prompt = promptNewBranch
		m.input = ""
		m.status = ""
		return m, nil
	case 'r':
		if m.currentRepo != nil && !m.prsLoading {
			m.prsLoading = true
			m.prsErr = ""
			m.status = "refreshing..."
			return m, fetchPRsCmd(m.currentRepo)
		}
	case 's':
		if m.prsCursor < len(items) {
			it := items[m.prsCursor]
			if it.Branch != "" {
				m.stopWS(branchKey(it.Branch))
			} else {
				m.stopWS(prKey(it.PR.Number))
			}
		}
		return m, nil
	case 'd':
		if m.prsCursor >= len(items) {
			return m, nil
		}
		it := items[m.prsCursor]
		var key wsKey
		var label string
		if it.Branch != "" {
			key = branchKey(it.Branch)
			label = ".branch-" + sanitizeBranch(it.Branch)
		} else {
			if !it.PR.InReview {
				return m, nil
			}
			key = prKey(it.PR.Number)
			label = fmt.Sprintf(".review%d", it.PR.Number)
		}
		if m.confirmDelete != key {
			m.confirmDelete = key
			m.status = "press d again to delete " + label
			return m, nil
		}
		m.confirmDelete = wsKey{}
		m.stopWS(key)
		var delErr error
		if it.Branch != "" {
			delErr = removeWorkspace(m.currentRepo, it.Path)
		} else {
			delErr = removeReview(m.currentRepo, it.PR.Number)
		}
		if delErr != nil {
			m.status = "delete failed: " + delErr.Error()
			return m, nil
		}
		delete(m.running, key)
		if it.Branch != "" {
			m.removeBranchByName(it.Branch)
		} else {
			pr := it.PR
			if pr.Status == "open" || pr.Status == "draft" {
				for i := range m.prs {
					if m.prs[i].Number == pr.Number {
						m.prs[i].InReview = false
					}
				}
			} else {
				for i := range m.prs {
					if m.prs[i].Number == pr.Number {
						m.prs = append(m.prs[:i], m.prs[i+1:]...)
						break
					}
				}
			}
		}
		if m.prsCursor >= len(m.visibleWorkspaces()) && m.prsCursor > 0 {
			m.prsCursor--
		}
		m.status = "deleted " + label
		return m, nil
	}
	return m, nil
}

// visibleWorkspaces returns branch rows (alphabetical) followed by PR rows in the
// existing descending-number order.
func (m model) visibleWorkspaces() []workspaceItem {
	out := make([]workspaceItem, 0, len(m.branches)+len(m.prs))
	for i := range m.branches {
		b := m.branches[i]
		out = append(out, workspaceItem{Branch: b.Branch, Path: b.Path})
	}
	for i := range m.prs {
		out = append(out, workspaceItem{PR: &m.prs[i]})
	}
	return out
}

// refreshBranches scans the repo's worktrees and rebuilds m.branches, filtering
// out any branch that already has a matching PR. For such branches the on-disk
// directory is migrated from .branch-<x> to .review<N> so subsequent PR-row
// interactions resolve to the right path.
func (m *model) refreshBranches() {
	if m.currentRepo == nil {
		m.branches = nil
		return
	}
	all := listWorkspaces(m.currentRepo)
	prByBranch := make(map[string]int, len(m.prs))
	for i, pr := range m.prs {
		if pr.HeadRef != "" {
			prByBranch[pr.HeadRef] = i
		}
	}
	var branches []workspace
	for _, w := range all {
		if w.Branch == "" {
			continue
		}
		if idx, ok := prByBranch[w.Branch]; ok {
			m.prs[idx].InReview = true
			// Rename the worktree to the .review<N> convention if it still
			// uses the .branch-* layout. Errors are non-fatal — the row still
			// appears, just at the old path.
			newPath := reviewPath(m.currentRepo, m.prs[idx].Number)
			if w.Path != newPath {
				if err := moveWorkspace(m.currentRepo, w.Path, newPath); err != nil && m.status == "" {
					m.status = "rename worktree: " + err.Error()
				}
			}
			continue
		}
		branches = append(branches, w)
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].Branch < branches[j].Branch })
	m.branches = branches
}

func (m *model) removeBranchByName(name string) {
	for i := range m.branches {
		if m.branches[i].Branch == name {
			m.branches = append(m.branches[:i], m.branches[i+1:]...)
			return
		}
	}
}

// transitionBranchToPR is called when the tick has detected that the branch
// the user is viewing now has a PR (either created by our push action or
// externally). It renames the worktree to .review<N>, rekeys the running map,
// and flips the actions screen into PR mode. It then refreshes the PR list so
// the row layout settles.
func (m model) transitionBranchToPR(branch string, prNumber int) (tea.Model, tea.Cmd) {
	newPath := reviewPath(m.currentRepo, prNumber)
	oldPath := m.actionsPath
	if oldPath != newPath {
		// refreshBranches may already have renamed the worktree; only attempt
		// the move when the old path still exists.
		if _, err := os.Stat(oldPath); err == nil {
			if err := moveWorkspace(m.currentRepo, oldPath, newPath); err != nil {
				m.status = "rename worktree: " + err.Error()
				return m, fetchPRsCmd(m.currentRepo)
			}
		}
	}
	oldKey := branchKey(branch)
	newKey := prKey(prNumber)
	if wsMap, ok := m.running[oldKey]; ok {
		m.running[newKey] = wsMap
		delete(m.running, oldKey)
	}
	m.removeBranchByName(branch)
	for i := range m.prs {
		if m.prs[i].Number == prNumber {
			m.prs[i].InReview = true
			break
		}
	}
	m.activeWS = newKey
	m.actionsPath = newPath
	m.branchCommits = 0
	m.status = fmt.Sprintf("PR #%d opened", prNumber)
	return m, fetchPRsCmd(m.currentRepo)
}

func (m model) openActionsPR(prNumber int) (tea.Model, tea.Cmd) {
	return m.enterActions(prKey(prNumber), reviewPath(m.currentRepo, prNumber))
}

func (m model) openActionsBranch(branch, path string) (tea.Model, tea.Cmd) {
	return m.enterActions(branchKey(branch), path)
}

func (m model) enterActions(key wsKey, path string) (tea.Model, tea.Cmd) {
	m.screen = screenActions
	m.activeWS = key
	m.actionsPath = path
	m.actionsCursor = 0
	m.branchCommits = 0
	cfg, err := loadRepoConfig(m.currentRepo.Path)
	if err != nil {
		m.actionsErr = err.Error()
		m.actions = nil
	} else {
		m.actions = cfg.Actions
		m.actionsErr = ""
	}
	var cmds []tea.Cmd
	if key.isPR() {
		if _, cached := m.previews[key.PRNumber]; !cached {
			if owner, repo, ok := m.currentRepo.ownerRepo(); ok {
				cmds = append(cmds, fetchPreviewsCmd(owner, repo, key.PRNumber))
			}
		}
	}
	if key.isBranch() {
		cmds = append(cmds, branchStatusCmd(key.Branch, path, m.prs))
		cmds = append(cmds, branchTickCmd(key))
	}
	if len(cmds) == 0 {
		return m, nil
	}
	return m, tea.Batch(cmds...)
}

// syntheticIndex returns the running-map index reserved for the synthetic
// "push and open PR" row. It sits beyond the user-action indices so it can't
// collide with them as the user adds/removes actions.
const syntheticIndex = -1

// hasSynthetic reports whether the actions list currently shows a synthetic
// top row (branch mode only).
func (m model) hasSynthetic() bool {
	return m.activeWS.isBranch()
}

func (m model) actionRowCount() int {
	n := len(m.actions)
	if m.hasSynthetic() {
		n++
	}
	return n
}

// userActionIndex converts a visible cursor index into m.actions index, or
// -1 if the cursor points at the synthetic row.
func (m model) userActionIndex(cursor int) int {
	if m.hasSynthetic() {
		if cursor == 0 {
			return -1
		}
		return cursor - 1
	}
	return cursor
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
		m.activeWS = wsKey{}
		m.actionsPath = ""
		m.branchCommits = 0
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
		if m.actionsCursor < m.actionRowCount()-1 {
			m.actionsCursor++
		}
	case 'n':
		m.prompt = promptNewName
		m.input = ""
		m.genErr = ""
		return m, nil
	case 'e':
		ui := m.userActionIndex(m.actionsCursor)
		if ui >= 0 && ui < len(m.actions) {
			m.prompt = promptEditDesc
			m.editIndex = ui
			m.input = ""
			m.genErr = ""
		}
		return m, nil
	case 'i':
		ui := m.userActionIndex(m.actionsCursor)
		if ui >= 0 && ui < len(m.actions) {
			m.inspecting = !m.inspecting
		}
		return m, nil
	case tea.KeyEnter:
		if m.hasSynthetic() && m.actionsCursor == 0 {
			if m.branchCommits == 0 {
				return m, nil
			}
			if r, ok := m.running[m.activeWS][syntheticIndex]; ok && !r.Done {
				return m, nil
			}
			m.runningExpand = false
			return m, pushAndOpenPRCmd(m.activeWS, m.actionsPath, m.activeWS.Branch)
		}
		ui := m.userActionIndex(m.actionsCursor)
		if ui < 0 || ui >= len(m.actions) {
			return m, nil
		}
		if r, ok := m.running[m.activeWS][ui]; ok && !r.Done {
			return m, nil
		}
		action := m.actions[ui]
		m.runningExpand = false
		return m, runActionCmd(action, m.activeWS, ui, m.actionsPath)
	case 's':
		var idx int
		if m.hasSynthetic() && m.actionsCursor == 0 {
			idx = syntheticIndex
		} else {
			idx = m.userActionIndex(m.actionsCursor)
		}
		if r, ok := m.running[m.activeWS][idx]; ok && !r.Done {
			r.Stopped = true
			killAction(r.cmd)
		}
		return m, nil
	}
	return m, nil
}

func (m model) moveAction(delta int) (tea.Model, tea.Cmd) {
	ui := m.userActionIndex(m.actionsCursor)
	if ui < 0 {
		return m, nil
	}
	uj := ui + delta
	if uj < 0 || uj >= len(m.actions) {
		return m, nil
	}
	m.actions[ui], m.actions[uj] = m.actions[uj], m.actions[ui]
	if wsMap := m.running[m.activeWS]; wsMap != nil {
		ri, hasI := wsMap[ui]
		rj, hasJ := wsMap[uj]
		delete(wsMap, ui)
		delete(wsMap, uj)
		if hasI {
			wsMap[uj] = ri
		}
		if hasJ {
			wsMap[ui] = rj
		}
	}
	if m.hasSynthetic() {
		m.actionsCursor = uj + 1
	} else {
		m.actionsCursor = uj
	}
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

func (m model) submitNewBranch(name string) (tea.Model, tea.Cmd) {
	m.prompt = promptNone
	m.input = ""
	if err := validateBranchName(name); err != nil {
		m.status = err.Error()
		return m, nil
	}
	if exists, err := branchExists(m.currentRepo, name); err != nil {
		m.status = "check existing branches: " + err.Error()
		return m, nil
	} else if exists {
		m.status = fmt.Sprintf("branch %q already exists locally or on origin", name)
		return m, nil
	}
	path, err := startBranch(m.currentRepo, name)
	if err != nil {
		m.status = "create branch: " + err.Error()
		return m, nil
	}
	m.branches = append(m.branches, workspace{Branch: name, Path: path})
	sort.Slice(m.branches, func(i, j int) bool { return m.branches[i].Branch < m.branches[j].Branch })
	m.status = "created branch " + name
	return m.openActionsBranch(name, path)
}

func (m model) submitPrompt(v string) (tea.Model, tea.Cmd) {
	switch m.prompt {
	case promptNewBranch:
		return m.submitNewBranch(v)
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
	draftIcon  = "⏹"
	branchIcon = "" // nf-oct-git_branch
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

	items := m.visibleWorkspaces()
	switch {
	case m.prsLoading && len(items) == 0 && m.prsErr == "":
		b.WriteString(dimStyle.Render("  loading...") + "\n")
	case m.prsErr != "":
		b.WriteString(warnStyle.Render("  "+m.prsErr) + "\n")
	case len(items) == 0:
		b.WriteString(dimStyle.Render("  no open PRs") + "\n")
	default:
		numWidth, agoWidth := 1, 0
		for _, it := range items {
			if it.PR == nil {
				continue
			}
			if w := len(strconv.Itoa(it.PR.Number)); w > numWidth {
				numWidth = w
			}
			if w := len(humanizeAgo(it.PR.CreatedAt)); w > agoWidth {
				agoWidth = w
			}
		}
		owner, repoName, ownerOK := "", "", false
		if m.currentRepo != nil {
			owner, repoName, ownerOK = m.currentRepo.ownerRepo()
		}
		for i, it := range items {
			cursor := "  "
			if i == m.prsCursor {
				cursor = cursorStyle.Render("▸ ")
			}
			if it.Branch != "" {
				mark := markStyle.Render("*") + " "
				if m.wsHasRunning(branchKey(it.Branch)) {
					mark = markStyle.Render("*") + flashStyle.Render("●")
				}
				agoPad := dimStyle.Render(strings.Repeat(" ", agoWidth))
				numPad := strings.Repeat(" ", numWidth)
				name := it.Branch
				if i == m.prsCursor {
					name = selectedStyle.Render(name)
				}
				icon := dimStyle.Render(branchIcon) + " "
				line := cursor + mark + " " + agoPad + "  " + icon + numPad + " " + name
				b.WriteString(line + "\n")
				continue
			}
			pr := it.PR
			mark := "  "
			if pr.InReview {
				if m.wsHasRunning(prKey(pr.Number)) {
					mark = markStyle.Render("*") + flashStyle.Render("●")
				} else {
					mark = markStyle.Render("*") + " "
				}
			}
			numStr := fmt.Sprintf("%*d", numWidth, pr.Number)
			ago := dimStyle.Render(fmt.Sprintf("%*s", agoWidth, humanizeAgo(pr.CreatedAt)))
			statusIcon := "  "
			switch pr.Status {
			case "merged":
				statusIcon = mergedStyle.Render(mergedIcon) + " "
			case "closed", "unknown":
				statusIcon = warnStyle.Render(closedIcon) + " "
			case "draft":
				statusIcon = dimStyle.Render(draftIcon) + " "
			}
			rest := ": " + pr.Title
			if i == m.prsCursor {
				numStr = selectedStyle.Render(numStr)
				rest = selectedStyle.Render(rest)
			}
			if ownerOK {
				numStr = hyperlink(fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repoName, pr.Number), numStr)
			}
			line := cursor + mark + " " + ago + "  " + statusIcon + numStr + rest
			if pr.Author != "" {
				line += "  " + authorStyle.Render("@"+pr.Author)
			}
			b.WriteString(line + "\n")
		}
	}

	if m.prompt == promptNewBranch {
		b.WriteString("\n" + renderPrompt(m))
	} else if m.status != "" {
		b.WriteString("\n" + flashStyle.Render(m.status) + "\n")
	}
	help := "↑/↓: navigate • enter: open • n: new branch • r: refresh • s: stop running • d: delete folder • esc: back • q: quit"
	if m.prompt == promptNewBranch {
		help = "enter: submit • esc: cancel"
	}
	b.WriteString("\n" + dimStyle.Render(help) + "\n")
	return b.String()
}

func (m model) renderActions() string {
	var b strings.Builder
	if m.currentRepo != nil {
		owner, repo, ownerOK := m.currentRepo.ownerRepo()
		switch {
		case m.activeWS.isBranch():
			label := m.currentRepo.URL
			if ownerOK {
				label = owner + "/" + repo
			}
			b.WriteString(titleStyle.Render(fmt.Sprintf("law - %s %s %s", githubIcon, label, m.activeWS.Branch)) + "\n")
		case ownerOK:
			b.WriteString(titleStyle.Render(fmt.Sprintf("law - %s %s/%s #%d", githubIcon, owner, repo, m.activeWS.PRNumber)) + "\n")
			b.WriteString(dimStyle.Render(fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, m.activeWS.PRNumber)) + "\n")
		default:
			b.WriteString(titleStyle.Render(fmt.Sprintf("law - %s %s #%d", githubIcon, m.currentRepo.URL, m.activeWS.PRNumber)) + "\n")
		}
		if m.activeWS.isPR() {
			for _, p := range m.previews[m.activeWS.PRNumber] {
				b.WriteString(hyperlink(p.URL, authorStyle.Render(previewIcon)+" preview: "+p.Name) + "\n")
			}
		}
		b.WriteString("\n")
	} else {
		b.WriteString(titleStyle.Render("law - actions") + "\n\n")
	}

	wsMap := m.running[m.activeWS]
	rowIdx := 0
	if m.hasSynthetic() {
		cursor := "  "
		if m.actionsCursor == 0 {
			cursor = cursorStyle.Render("▸ ")
		}
		var label string
		if m.branchCommits == 0 {
			label = dimStyle.Render("waiting for commits")
		} else {
			label = "push and open PR"
			if m.actionsCursor == 0 {
				label = selectedStyle.Render(label)
			}
		}
		b.WriteString(cursor + runMarker(wsMap[syntheticIndex]) + " " + label + "\n")
		rowIdx = 1
	}

	switch {
	case m.actionsErr != "":
		b.WriteString(warnStyle.Render("  "+m.actionsErr) + "\n")
	case len(m.actions) == 0 && !m.hasSynthetic():
		b.WriteString(dimStyle.Render("  no actions configured") + "\n")
		if m.currentRepo != nil {
			b.WriteString(dimStyle.Render("  define them in "+repoConfigPath(m.currentRepo.Path)) + "\n")
		}
	default:
		for i, a := range m.actions {
			cursor := "  "
			text := a.Name
			if rowIdx+i == m.actionsCursor {
				cursor = cursorStyle.Render("▸ ")
				text = selectedStyle.Render(text)
			}
			b.WriteString(cursor + runMarker(wsMap[i]) + " " + text + "\n")
		}
	}

	ui := m.userActionIndex(m.actionsCursor)
	if m.inspecting && ui >= 0 && ui < len(m.actions) {
		b.WriteString("\n" + renderInspectPanel(m.actions[ui]))
	}

	var activeRunning *runningAction
	if m.hasSynthetic() && m.actionsCursor == 0 {
		activeRunning = wsMap[syntheticIndex]
	} else if ui >= 0 {
		activeRunning = wsMap[ui]
	}
	if activeRunning != nil {
		b.WriteString("\n" + renderRunningPanel(activeRunning, m.runningExpand))
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
		if activeRunning != nil {
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
	case promptNewBranch:
		label = "name for new branch (slashes encouraged, e.g. dom/fix-this):"
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
