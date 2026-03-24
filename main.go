package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	listPanesOutput = func() ([]byte, error) {
		return tmuxCommand("list-panes", "-a",
			"-F", "#{session_name}:#{window_index} #{pane_pid} #{window_active}").Output()
	}
	capturePaneOutput = func(window string) ([]byte, error) {
		return tmuxCommand("capture-pane", "-t", window, "-p", "-e").Output()
	}
)

const tmuxSocketEnv = "TMUX_AI_STATUS_SOCKET"

var (
	// lastActive tracks when each window was last seen as active.
	// Prevents flashing during spinner redraws.
	lastActive   = make(map[string]time.Time)
	lastActiveMu sync.Mutex
	activeGrace  = 10 * time.Second

	// statusState tracks per-window status with hysteresis.
	// A new status must be seen for 2 consecutive cycles before being applied,
	// preventing flicker from brief child processes or spinner redraws.
	statusState   = make(map[string]*windowState)
	statusStateMu sync.Mutex

	// initialBaselineComplete becomes true after the first successful scan.
	// Existing windows should establish a read baseline on startup rather than
	// immediately turning into unread/mailbox state after a binary restart.
	initialBaselineComplete bool
)

var spinnerElapsedRE = regexp.MustCompile(`\(\s*\d+[hms](?:\s+\d+[hms]){0,2}`)
var thoughtElapsedRE = regexp.MustCompile(`\(thought for \d+`)
var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)
var localAgentsRE = regexp.MustCompile(`\b[1-9]\d*\s+local agents?\b`)
var backgroundTasksRE = regexp.MustCompile(`\b[1-9]\d*\s+background tasks?\s+still running\b`)
var elapsedCompletionRE = regexp.MustCompile(`^[─✻*] [A-Z][A-Za-z]+ed for `)

const unreadIdleThreshold = 3 // cycles (3 * 2s = 6s) before showing 📬 on idle
const claudeIdleCooldown = 15 * time.Second
const bottomActivityScanLines = 20
const topActivityScanLines = 6

type agentPolicy struct {
	name                string
	statusPrefix        string
	draftPrefix         string
	unreadIdleThreshold int
	idleCooldown        time.Duration
}

func (p agentPolicy) known() bool {
	return p.statusPrefix != ""
}

func (p agentPolicy) status(emoji string) string {
	return p.statusPrefix + emoji
}

func (p agentPolicy) draftStatus() string {
	return p.draftPrefix + "✍️"
}

var (
	codexPolicy = agentPolicy{
		name:                "codex",
		statusPrefix:        "x ",
		draftPrefix:         "x",
		unreadIdleThreshold: 0,
	}
	claudePolicy = agentPolicy{
		name:                "claude",
		statusPrefix:        "c ",
		draftPrefix:         "c",
		unreadIdleThreshold: unreadIdleThreshold,
		idleCooldown:        claudeIdleCooldown,
	}
)

type windowState struct {
	applied string // status currently shown in tmux
	pending string // candidate status seen last cycle
	count   int    // consecutive cycles pending has been seen
	unread  bool   // agent finished work while window was unfocused
}

const stabilityThreshold = 1 // cycles a new status must hold before applying

func main() {
	for {
		updateAllPanes()
		time.Sleep(2 * time.Second)
	}
}

type paneInfo struct {
	window  string
	pid     int
	focused bool
}

// Unread tracking: detect when agent finishes work while user isn't looking.
var (
	windowWasWorking = make(map[string]bool)
	windowSeen       = make(map[string]bool)
	windowPromptSig  = make(map[string]string)
	windowDoneSig    = make(map[string]string)
	windowIdleStreak = make(map[string]int)
	windowLastWorkAt = make(map[string]time.Time)
	windowActive     = make(map[string]activeMarkerState)
)

type activeMarkerState struct {
	sig     string
	paneSig string
	started time.Time
}

func listPanes() []paneInfo {
	out, err := listPanesOutput()
	if err != nil {
		return nil
	}
	var panes []paneInfo
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		panes = append(panes, paneInfo{
			window:  fields[0],
			pid:     pid,
			focused: fields[2] == "1",
		})
	}
	return panes
}

type paneCapture struct {
	content string
	styled  string
	ok      bool
}

func getPaneContent(window string, cache map[string]*paneCapture) (string, bool) {
	if c, ok := cache[window]; ok {
		return c.content, c.ok
	}

	out, err := capturePaneOutput(window)
	if err != nil {
		cache[window] = &paneCapture{ok: false}
		return "", false
	}

	styled := string(out)
	content := stripANSI(styled)
	cache[window] = &paneCapture{content: content, styled: styled, ok: true}
	return content, true
}

func getPaneStyledContent(window string, cache map[string]*paneCapture) (string, bool) {
	if c, ok := cache[window]; ok {
		return c.styled, c.ok
	}
	if _, ok := getPaneContent(window, cache); !ok {
		return "", false
	}
	if c, ok := cache[window]; ok {
		return c.styled, c.ok
	}
	return "", false
}

func updateAllPanes() {
	panes := listPanes()
	if len(panes) == 0 {
		return
	}

	childMap := buildChildMap()
	seenWindows := make(map[string]bool)
	paneCache := make(map[string]*paneCapture)

	// Group panes by window — pick the most significant status per window.
	type windowSummary struct {
		status  string
		focused bool
	}
	summaries := make(map[string]*windowSummary)

	for _, p := range panes {
		seenWindows[p.window] = true
		rawStatus := getStatus(p.window, p.pid, childMap, paneCache)
		prev, exists := summaries[p.window]
		if !exists {
			summaries[p.window] = &windowSummary{status: rawStatus, focused: p.focused}
		} else {
			prev.focused = prev.focused || p.focused
			if statusPriority(rawStatus) > statusPriority(prev.status) {
				prev.status = rawStatus
			}
		}
	}

	// Apply unread logic per window, then set status.
	initialScan := !initialBaselineComplete
	for window, s := range summaries {
		now := time.Now()
		rawStatus := s.status
		focused := s.focused
		wasWorking := windowWasWorking[window]
		isWorking := isWorkingStatus(rawStatus)
		seenBefore := windowSeen[window]
		allowInitialPromptUnread := !initialScan || seenBefore
		promptSig := ""
		doneSig := ""
		liveDraftSig := ""
		if !isWorking && rawStatus != "" {
			promptSig, doneSig = paneSignals(window, paneCache)
			liveDraftSig = paneLiveDraftSignature(window, paneCache)
		}
		prevPromptSig := windowPromptSig[window]
		prevDoneSig := windowDoneSig[window]

		// Mark unread only for meaningful events:
		// - working -> idle completion while unfocused
		// - new completion/prompt signature after initial baseline
		if shouldMarkUnread(
			wasWorking,
			focused,
			isWorking,
			rawStatus,
			seenBefore,
			promptSig,
			prevPromptSig,
			doneSig,
			prevDoneSig,
			allowInitialPromptUnread,
			liveDraftSig != "",
		) {
			markUnread(window)
		}
		// User focused the window → clear unread
		if focused {
			clearUnread(window)
		}
		// Agent started working again → clear unread
		if isWorking {
			clearUnread(window)
			windowLastWorkAt[window] = now
		}

		if !isWorking && rawStatus != "" && strings.HasSuffix(rawStatus, "💤") {
			windowIdleStreak[window]++
		} else {
			windowIdleStreak[window] = 0
		}

		sinceLastWork := claudeIdleCooldown
		if ts, ok := windowLastWorkAt[window]; ok {
			sinceLastWork = now.Sub(ts)
		}

		windowWasWorking[window] = isWorking
		windowSeen[window] = true
		windowPromptSig[window] = promptSig
		windowDoneSig[window] = doneSig

		effectiveStatus := resolveDisplayStatus(
			rawStatus,
			liveDraftSig,
			doneSig,
			isUnread(window),
			windowIdleStreak[window],
			sinceLastWork,
		)

		setWindowStatus(window, effectiveStatus)
	}

	// Clean up stale entries
	lastActiveMu.Lock()
	for w := range lastActive {
		if !seenWindows[w] {
			delete(lastActive, w)
		}
	}
	lastActiveMu.Unlock()
	statusStateMu.Lock()
	for w := range statusState {
		if !seenWindows[w] {
			delete(statusState, w)
		}
	}
	statusStateMu.Unlock()
	for w := range windowWasWorking {
		if !seenWindows[w] {
			delete(windowWasWorking, w)
		}
	}
	for w := range windowSeen {
		if !seenWindows[w] {
			delete(windowSeen, w)
		}
	}
	for w := range windowPromptSig {
		if !seenWindows[w] {
			delete(windowPromptSig, w)
		}
	}
	for w := range windowDoneSig {
		if !seenWindows[w] {
			delete(windowDoneSig, w)
		}
	}
	for w := range windowIdleStreak {
		if !seenWindows[w] {
			delete(windowIdleStreak, w)
		}
	}
	for w := range windowLastWorkAt {
		if !seenWindows[w] {
			delete(windowLastWorkAt, w)
		}
	}
	for w := range windowActive {
		if !seenWindows[w] {
			delete(windowActive, w)
		}
	}

	initialBaselineComplete = true
}

func isWorkingStatus(status string) bool {
	return status != "" && !strings.HasSuffix(status, "💤")
}

func statusPriority(status string) int {
	if isWorkingStatus(status) {
		return 2
	}
	if status != "" {
		return 1
	}
	return 0
}

func markUnread(window string) {
	statusStateMu.Lock()
	defer statusStateMu.Unlock()
	ws, ok := statusState[window]
	if !ok {
		ws = &windowState{}
		statusState[window] = ws
	}
	ws.unread = true
}

func clearUnread(window string) {
	statusStateMu.Lock()
	defer statusStateMu.Unlock()
	if ws, ok := statusState[window]; ok {
		ws.unread = false
	}
}

func isUnread(window string) bool {
	statusStateMu.Lock()
	defer statusStateMu.Unlock()
	if ws, ok := statusState[window]; ok {
		return ws.unread
	}
	return false
}

func shouldMarkUnread(
	wasWorking, focused, isWorking bool,
	rawStatus string,
	seenBefore bool,
	promptSig, prevPromptSig, doneSig, prevDoneSig string,
	allowInitialPromptUnread bool,
	isLiveDraft bool,
) bool {
	if focused || isWorking || rawStatus == "" {
		return false
	}
	// Live draft edits in the prompt (unsent user input) should not create unread.
	if isLiveDraft && !wasWorking && doneSig == "" {
		return false
	}
	if wasWorking {
		return true
	}
	if !seenBefore {
		// First baseline should stay read for bare prompts, but explicit
		// prompt text ("› Run /review...") indicates immediate attention for
		// panes first seen during this process lifetime. On startup, treat the
		// first scan as read baseline so restarts do not reset everything to 📬.
		return allowInitialPromptUnread && hasPromptText(promptSig)
	}
	if doneSig != "" && doneSig != prevDoneSig {
		return true
	}
	if promptSig != "" && promptSig != prevPromptSig {
		return true
	}
	return false
}

func hasPromptText(promptSig string) bool {
	if promptSig == "" {
		return false
	}

	if strings.HasPrefix(promptSig, "codex:") {
		p := strings.TrimSpace(strings.TrimPrefix(promptSig, "codex:"))
		return p != "" && p != "›"
	}
	if strings.HasPrefix(promptSig, "claude:") {
		p := strings.TrimSpace(strings.TrimPrefix(promptSig, "claude:"))
		return p != "" && p != "❯"
	}
	return false
}

// setWindowStatus applies hysteresis: a new status must be seen for
// stabilityThreshold consecutive cycles before the tmux tab is updated.
func setWindowStatus(window, status string) {
	statusStateMu.Lock()
	defer statusStateMu.Unlock()

	ws, ok := statusState[window]
	if !ok {
		ws = &windowState{}
		statusState[window] = ws
	}

	// Already showing this status — nothing to do
	if status == ws.applied {
		ws.pending = ""
		ws.count = 0
		return
	}

	// New candidate status
	if status == ws.pending {
		ws.count++
	} else {
		ws.pending = status
		ws.count = 1
	}

	// Only apply once stable
	if ws.count < stabilityThreshold {
		return
	}

	ws.applied = status
	ws.pending = ""
	ws.count = 0

	if status != "" {
		tmuxCommand("rename-window", "-t", window, status).Run()
	} else {
		tmuxCommand("set-option", "-t", window, "automatic-rename", "on").Run()
	}
}

func tmuxCommand(args ...string) *exec.Cmd {
	return exec.Command("tmux", tmuxArgs(args...)...)
}

func tmuxArgs(args ...string) []string {
	if socket := strings.TrimSpace(os.Getenv(tmuxSocketEnv)); socket != "" {
		prefixed := []string{"-S", socket}
		return append(prefixed, args...)
	}
	return args
}

func buildChildMap() map[int][]int {
	m := make(map[int][]int)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return m
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid := readPPID(pid)
		if ppid > 0 {
			m[ppid] = append(m[ppid], pid)
		}
	}
	return m
}

func readPPID(pid int) int {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	return parsePPIDFromStat(string(data))
}

func parsePPIDFromStat(stat string) int {
	i := strings.LastIndex(stat, ")")
	if i < 0 || i+2 >= len(stat) {
		return 0
	}
	fields := strings.Fields(stat[i+2:])
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}

func getStatus(window string, panePID int, childMap map[int][]int, paneCache map[string]*paneCapture) string {
	agentPID, agentName := findAgent(panePID, childMap)
	if agentPID == 0 {
		return ""
	}

	policy := policyForAgentName(agentName)
	hasBackgroundAgents := paneHasBackgroundAgents(window, paneCache)

	descendants := collectDescendants(agentPID, childMap)

	var childSignals []string
	for _, d := range descendants {
		comm := strings.ToLower(readComm(d))
		cmdline := strings.ToLower(readCmdline(d))
		if isAgentLikeProcess(comm, cmdline) {
			continue
		}
		signal := cmdline
		if signal == "" {
			signal = comm
		}
		if signal == "" {
			continue
		}
		childSignals = append(childSignals, signal)
	}

	if len(childSignals) > 0 {
		childStatus := classifyChildren(childSignals)
		if childStatus == "⚙️" {
			if hasBackgroundAgents {
				return policy.status("🤖")
			}
			return unknownChildStatus(
				policy,
				isPaneActive(window, paneCache),
				paneNeedsAttention(window, paneCache),
			)
		}
		return policy.status(childStatus)
	}
	if hasBackgroundAgents {
		return policy.status("🤖")
	}

	// Active spinner always takes priority — both Claude and Codex can
	// show a spinner and a prompt simultaneously (Codex shows the prompt
	// at the bottom while working above it).
	if isPaneActive(window, paneCache) {
		return policy.status("🧠")
	}
	if paneNeedsAttention(window, paneCache) {
		return policy.status("💤")
	}
	return policy.status("💤")
}

func unknownChildStatus(policy agentPolicy, paneActive, needsAttention bool) string {
	if paneActive {
		return policy.status("🧠")
	}
	if needsAttention {
		return policy.status("💤")
	}
	return policy.status("⚙️")
}

func paneNeedsAttention(window string, paneCache map[string]*paneCapture) bool {
	content, ok := getPaneContent(window, paneCache)
	if !ok {
		return false
	}
	return classifyPaneNeedsAttention(content)
}

func paneHasBackgroundAgents(window string, paneCache map[string]*paneCapture) bool {
	content, ok := getPaneContent(window, paneCache)
	if !ok {
		return false
	}
	return classifyPaneBackgroundAgents(content)
}

func paneSignals(window string, paneCache map[string]*paneCapture) (promptSig, doneSig string) {
	content, ok := getPaneStyledContent(window, paneCache)
	if !ok {
		return "", ""
	}
	plain := stripANSI(content)
	return detectMeaningfulPromptSignature(content), classifyPaneCompletionSignature(plain)
}

func paneLiveDraftSignature(window string, paneCache map[string]*paneCapture) string {
	content, ok := getPaneStyledContent(window, paneCache)
	if !ok {
		return ""
	}
	return detectLiveDraftSignature(content)
}

// isPaneActive captures the pane content and checks for activity indicators.
// Uses a grace period to prevent flashing during spinner redraws.
func isPaneActive(window string, paneCache map[string]*paneCapture) bool {
	now := time.Now()
	active := false

	if content, ok := getPaneContent(window, paneCache); ok {
		active = classifyPaneContent(content)
		if active {
			styled, _ := getPaneStyledContent(window, paneCache)
			active = !isStaleActiveMarker(window, content, styled, now)
		} else {
			clearActiveMarker(window)
		}
	} else {
		clearActiveMarker(window)
	}

	lastActiveMu.Lock()
	defer lastActiveMu.Unlock()

	if active {
		lastActive[window] = now
		return true
	}

	// Not detected as active right now — check grace period
	if last, ok := lastActive[window]; ok {
		if now.Sub(last) < activeGrace {
			return true
		}
		delete(lastActive, window)
	}
	return false
}

const staleActiveThreshold = 12 * time.Second
const barePromptStaleActiveThreshold = 2 * time.Minute

func isStaleActiveMarker(window, content, styled string, now time.Time) bool {
	activeSig := classifyPaneActiveSignature(content)
	if activeSig == "" {
		return false
	}
	promptSig := detectPromptSignature(content)
	paneSig := activePaneSignature(content, styled)
	if promptSig == "" {
		setActiveMarker(window, activeSig, paneSig, now)
		return false
	}

	state, ok := windowActive[window]
	if !ok || state.sig != activeSig || state.paneSig != paneSig {
		setActiveMarker(window, activeSig, paneSig, now)
		return false
	}
	if state.started.IsZero() {
		setActiveMarker(window, activeSig, paneSig, now)
		return false
	}
	return now.Sub(state.started) >= activeMarkerThreshold(promptSig)
}

func clearActiveMarker(window string) {
	delete(windowActive, window)
}

func setActiveMarker(window, activeSig, paneSig string, now time.Time) {
	windowActive[window] = activeMarkerState{
		sig:     activeSig,
		paneSig: paneSig,
		started: now,
	}
}

func activeMarkerThreshold(promptSig string) time.Duration {
	if hasPromptText(promptSig) {
		return staleActiveThreshold
	}
	return barePromptStaleActiveThreshold
}

func activePaneSignature(content, styled string) string {
	if styled != "" {
		return strings.TrimRight(styled, "\n")
	}
	return strings.TrimRight(content, "\n")
}

func resolveDisplayStatus(
	rawStatus, draftSig, doneSig string,
	unread bool,
	idleStreak int,
	sinceLastWork time.Duration,
) string {
	// Final status resolution order matters:
	// 1. typed prompt text becomes drafting,
	// 2. Claude idle smoothing can temporarily hold 🧠 unless a real
	//    completion marker is visible,
	// 3. unread replaces stable idle once the pane has truly settled.
	status := withDraftMarker(rawStatus, draftSig)
	status = smoothIdleStatus(status, doneSig, sinceLastWork)
	return withUnreadMarker(status, isWorkingStatus(status), unread, idleStreak)
}

func withUnreadMarker(
	rawStatus string,
	isWorking, unread bool,
	idleStreak int,
) string {
	if isWorking || rawStatus == "" || !unread || !strings.HasSuffix(rawStatus, "💤") {
		return rawStatus
	}

	policy := policyForStatus(rawStatus)
	if policy.known() && idleStreak >= policy.unreadIdleThreshold {
		return policy.status("📬")
	}
	return rawStatus
}

func smoothIdleStatus(status, doneSig string, sinceLastWork time.Duration) string {
	// Claude panes can briefly expose a bare prompt while still in an active run.
	// Hold idle transitions for a short cooldown to prevent 🧠 <-> 💤 flicker.
	// Skip if user is drafting (✍️) — draft marker takes priority over smoothing.
	if strings.Contains(status, "✍️") {
		return status
	}
	if doneSig != "" {
		return status
	}
	policy := policyForStatus(status)
	if policy.known() &&
		policy.idleCooldown > 0 &&
		strings.HasSuffix(status, "💤") &&
		sinceLastWork < policy.idleCooldown {
		return policy.status("🧠")
	}
	return status
}

func withDraftMarker(status, draftSig string) string {
	// Unsent prompt text means "drafting" rather than unread/idle.
	if draftSig == "" || !hasPromptText(draftSig) {
		return status
	}
	policy := policyForStatus(status)
	if policy.known() && (strings.HasSuffix(status, "💤") || strings.HasSuffix(status, "📬")) {
		return policy.draftStatus()
	}
	return status
}

func policyForAgentName(name string) agentPolicy {
	if name == codexPolicy.name {
		return codexPolicy
	}
	return claudePolicy
}

func policyForStatus(status string) agentPolicy {
	switch {
	case strings.HasPrefix(status, codexPolicy.statusPrefix), strings.HasPrefix(status, codexPolicy.draftStatus()):
		return codexPolicy
	case strings.HasPrefix(status, claudePolicy.statusPrefix), strings.HasPrefix(status, claudePolicy.draftStatus()):
		return claudePolicy
	default:
		return agentPolicy{}
	}
}

func detectLiveDraftSignature(content string) string {
	lines := strings.Split(content, "\n")
	checked := 0
	// Track non-chrome, non-continuation lines below the prompt.
	// Prompt continuation lines (2-space indent wrapping) and footer
	// chrome don't count — only real agent output does.
	significantBelow := 0

	for i := len(lines) - 1; i >= 0 && checked < 12; i-- {
		rawLine := lines[i]
		plainLine := strings.TrimSpace(stripANSI(rawLine))
		if plainLine == "" {
			continue
		}
		checked++

		sig := ""
		switch {
		case strings.HasPrefix(plainLine, "›"):
			sig = "codex:" + plainLine
		case strings.HasPrefix(plainLine, "❯"):
			sig = "claude:" + plainLine
		}
		if sig != "" {
			if hasPromptText(sig) && significantBelow == 0 {
				if !hasTypedPromptText(rawLine) {
					return ""
				}
				return sig
			}
			return ""
		}
		if !isLiveDraftFooterLine(plainLine) && !isPromptContinuationLine(rawLine) {
			significantBelow++
		}
	}
	return ""
}

// isPromptContinuationLine detects wrapped continuation lines of a multi-line
// Claude/Codex prompt. These are indented with 2 spaces and contain plain user
// text (no agent-output markers like ⎿, ─, spinner prefixes, etc.).
func isPromptContinuationLine(rawLine string) bool {
	plain := stripANSI(rawLine)
	// Must start with exactly 2 spaces (Claude's continuation indent).
	if !strings.HasPrefix(plain, "  ") {
		return false
	}
	trimmed := strings.TrimSpace(plain)
	if trimmed == "" {
		return false
	}
	// Agent output uses structural prefixes — continuation lines don't.
	if strings.HasPrefix(trimmed, "⎿") ||
		strings.HasPrefix(trimmed, "│") ||
		strings.HasPrefix(trimmed, "·") ||
		strings.HasPrefix(trimmed, "•") ||
		strings.HasPrefix(trimmed, "●") {
		return false
	}
	return true
}

func isLiveDraftFooterLine(line string) bool {
	// Claude keeps prompt chrome below the editable line.
	if strings.Trim(line, "─") == "" {
		return true
	}
	if strings.HasPrefix(line, "⏵⏵ ") {
		return true
	}
	// Mode indicators (💬 chat, 🔧 tool, etc.) shown below the prompt.
	if strings.HasPrefix(line, "💬") ||
		strings.HasPrefix(line, "🔧") ||
		strings.HasPrefix(line, "🪄") {
		return true
	}
	return strings.HasPrefix(line, "🟢 ") ||
		strings.HasPrefix(line, "🟡 ") ||
		strings.HasPrefix(line, "🟠 ") ||
		strings.HasPrefix(line, "🔴 ")
}

func stripANSI(s string) string {
	return ansiEscapeRE.ReplaceAllString(s, "")
}

func hasTypedPromptText(rawLine string) bool {
	if !strings.Contains(rawLine, "\x1b[") {
		plain := strings.TrimSpace(rawLine)
		switch {
		case strings.HasPrefix(plain, "›"):
			return strings.TrimSpace(strings.TrimPrefix(plain, "›")) != ""
		case strings.HasPrefix(plain, "❯"):
			return strings.TrimSpace(strings.TrimPrefix(plain, "❯")) != ""
		default:
			return false
		}
	}

	dim := false
	reverse := false
	seenPrompt := false
	sawAny := false
	sawSolid := false
	sawDim := false

	for i := 0; i < len(rawLine); {
		if rawLine[i] == 0x1b && i+1 < len(rawLine) && rawLine[i+1] == '[' {
			j := i + 2
			for j < len(rawLine) && rawLine[j] != 'm' {
				j++
			}
			if j < len(rawLine) && rawLine[j] == 'm' {
				codes := strings.Split(rawLine[i+2:j], ";")
				if len(codes) == 1 && codes[0] == "" {
					codes = []string{"0"}
				}
				for _, code := range codes {
					switch code {
					case "0":
						dim = false
						reverse = false
					case "2":
						dim = true
					case "22":
						dim = false
					case "7":
						reverse = true
					case "27":
						reverse = false
					}
				}
				i = j + 1
				continue
			}
		}

		r, size := utf8.DecodeRuneInString(rawLine[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		i += size

		if !seenPrompt {
			if r == '›' || r == '❯' {
				seenPrompt = true
			}
			continue
		}
		if unicode.IsSpace(r) {
			continue
		}

		sawAny = true
		if dim {
			sawDim = true
			continue
		}
		if reverse {
			continue
		}
		sawSolid = true
	}

	if !sawAny {
		return false
	}
	if sawSolid {
		return true
	}
	if sawDim {
		return false
	}
	return false
}

// classifyPaneContent returns true if the pane content indicates active work.
// Scans the last 20 non-empty lines (bottom) and first 6 non-empty lines (top)
// because Claude's task-list view can push the live spinner deeper above the
// prompt/footer chrome while still keeping it in the current active block.
func classifyPaneContent(content string) bool {
	lines := strings.Split(content, "\n")

	// Bottom scan (last 20 non-empty lines) — catches task-list layouts where
	// the spinner sits above a longer list of pending work and footer chrome.
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < bottomActivityScanLines; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++

		// Explicit completion markers mean the run is done.
		if isCompletionLine(line) {
			return false
		}
		if hasActiveMarker(line) {
			return true
		}
	}

	// Top scan (first 6 non-empty lines) — catches task-list spinner.
	checked = 0
	for i := 0; i < len(lines) && checked < topActivityScanLines; i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++
		if hasActiveMarker(line) {
			return true
		}
	}

	return false
}

func hasSpinnerMarker(line string) bool {
	if len(line) < 2 {
		return false
	}
	// Fast path: check common ASCII spinner.
	if line[0] == '*' && line[1] == ' ' {
		return true
	}
	// Claude/Codex use a rotating set of Unicode spinner characters.
	// Rather than enumerating every variant, decode the first rune and
	// check if it's followed by a space and belongs to a known set.
	r, size := utf8.DecodeRuneInString(line)
	if r == utf8.RuneError || size+1 > len(line) || line[size] != ' ' {
		return false
	}
	switch r {
	case '·', '•', '●', '○', '◦', '◉', '◎', // dots/circles
		'⏺',                // record symbol
		'✢', '✣', '✤', '✥', // cross marks
		'✦', '✧', '✨', // stars (4-point)
		'✩', '✪', '✫', '✬', '✭', '✮', '✯', // stars (5-point)
		'✰',                                                                       // shadowed star
		'✱', '✲', '✳', '✴', '✵', '✶', '✷', '✸', '✹', '✺', '✻', '✼', '✽', '✾', '✿', // asterisks/florettes
		'❀', '❁', '❂', '❃', '❇', '❈', '❉', '❊', '❋': // more florettes/sparkles
		return true
	}
	return false
}

func hasActiveMarker(line string) bool {
	if strings.Contains(line, "esc to interrupt") {
		return true
	}
	if !hasSpinnerMarker(line) {
		return false
	}
	// Claude/Codex spinner verbs: "Thinking…", "Brewing...", "Perusing…", etc.
	if strings.Contains(line, "ing\u2026") || strings.Contains(line, "ing...") {
		return true
	}
	// Elapsed timing patterns:
	// "◦ Investigating ... (1m 08s • esc …)"  — spinnerElapsedRE
	// "· Fixing store logos… (thought for 8s)" — thoughtElapsedRE
	// "✶ Fixing store logos… (8m 52s · thinking)" — spinnerElapsedRE
	if spinnerElapsedRE.MatchString(line) || thoughtElapsedRE.MatchString(line) {
		return true
	}
	// Claude can also collapse active lines down to a static "(thinking)"
	// suffix without elapsed time while the task is still running.
	if strings.Contains(line, "thinking)") {
		return true
	}
	// Claude Agent() subprocesses: "● Agent(Download logos)"
	if strings.Contains(line, "Agent(") {
		return true
	}
	return false
}

func classifyPaneActiveSignature(content string) string {
	lines := strings.Split(content, "\n")
	// Bottom scan.
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < bottomActivityScanLines; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++
		if hasActiveMarker(line) {
			return line
		}
	}
	// Top scan.
	checked = 0
	for i := 0; i < len(lines) && checked < topActivityScanLines; i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++
		if hasActiveMarker(line) {
			return line
		}
	}
	return ""
}

// classifyPaneNeedsAttention returns true when the pane appears to be
// waiting for user input (prompt visible) rather than actively working.
func classifyPaneNeedsAttention(content string) bool {
	return classifyPaneAttentionSignature(content) != ""
}

func classifyPaneAttentionSignature(content string) string {
	if classifyPaneBackgroundAgents(content) {
		return ""
	}
	if classifyPaneContent(content) {
		return ""
	}
	return detectPromptSignature(content)
}

func classifyPaneBackgroundAgents(content string) bool {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 12; i-- {
		line := strings.TrimSpace(strings.ToLower(lines[i]))
		if line == "" {
			continue
		}
		checked++
		if localAgentsRE.MatchString(line) ||
			backgroundTasksRE.MatchString(line) ||
			isBackgroundManageLine(line) ||
			isBackgroundRunningFooterLine(line) {
			return true
		}
	}
	return false
}

func isBackgroundManageLine(line string) bool {
	// Claude sometimes reports a single managed background command without
	// the numbered "N background tasks still running" summary.
	return strings.Contains(line, "running in the background") &&
		strings.Contains(line, "to manage")
}

func isBackgroundRunningFooterLine(line string) bool {
	// Claude can keep a managed background command in the prompt footer:
	// "⏵⏵ bypass permissions on · <command> (running)"
	return strings.HasPrefix(line, "⏵⏵ ") &&
		strings.Contains(line, "·") &&
		strings.HasSuffix(line, "(running)")
}

func detectPromptSignature(content string) string {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 12; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++
		if strings.HasPrefix(line, "›") {
			return "codex:" + line
		}
		if strings.HasPrefix(line, "❯") {
			return "claude:" + line
		}
	}
	return ""
}

// detectMeaningfulPromptSignature is stricter than detectPromptSignature:
// it collapses dim/reverse ghost suggestions down to a bare prompt so they
// do not create unread markers.
func detectMeaningfulPromptSignature(content string) string {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 12; i-- {
		rawLine := lines[i]
		line := strings.TrimSpace(stripANSI(rawLine))
		if line == "" {
			continue
		}
		checked++
		if strings.HasPrefix(line, "›") {
			if hasTypedPromptText(rawLine) {
				return "codex:" + line
			}
			return "codex:›"
		}
		if strings.HasPrefix(line, "❯") {
			if hasTypedPromptText(rawLine) {
				return "claude:" + line
			}
			return "claude:❯"
		}
	}
	return ""
}

func classifyPaneCompletionSignature(content string) string {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 20; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		checked++
		if isCompletionLine(line) {
			return line
		}
	}
	return ""
}

func isCompletionLine(line string) bool {
	return elapsedCompletionRE.MatchString(line) ||
		line == "Done." || strings.HasPrefix(line, "Done. ") ||
		line == "All set." || strings.HasPrefix(line, "All set. ")
}

func findAgent(panePID int, childMap map[int][]int) (int, string) {
	for _, child := range childMap[panePID] {
		cmdline := readCmdline(child)
		lower := strings.ToLower(cmdline)
		if strings.Contains(lower, "claude") {
			return child, "claude"
		}
		if strings.Contains(lower, "codex") {
			return child, "codex"
		}
		for _, gc := range childMap[child] {
			cmdline = readCmdline(gc)
			lower = strings.ToLower(cmdline)
			if strings.Contains(lower, "claude") {
				return gc, "claude"
			}
			if strings.Contains(lower, "codex") {
				return gc, "codex"
			}
		}
	}
	return 0, ""
}

func collectDescendants(pid int, childMap map[int][]int) []int {
	var result []int
	queue := append([]int{}, childMap[pid]...)
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		result = append(result, p)
		queue = append(queue, childMap[p]...)
	}
	return result
}

func readCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(data), "\x00", " ")
}

func readComm(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func classifyChildren(names []string) string {
	joined := strings.ToLower(strings.Join(names, "\n"))

	if containsAny(
		joined,
		"make", "gcc", "g++", "cc1", "rustc", "javac", "tsc", "webpack", "vite", "esbuild", "rollup",
		"agent-build-coordinator/cli.ts build", "coordinator/cli.ts build", " next build", "npm run build", "pnpm run build", "yarn build", "go build", "cargo build",
	) {
		return "🔨"
	}
	if containsAny(joined, "jest", "vitest", "pytest", "mocha", "phpunit", "rspec") {
		return "🧪"
	}
	if containsAny(joined, "npm", "yarn", "pnpm", "pip", "apt", "brew", "pacman") {
		return "📦"
	}
	if containsAny(joined, "git") {
		return "🔀"
	}
	if containsAny(joined, "curl", "wget") {
		return "🌐"
	}
	return "⚙️"
}

func isAgentLikeProcess(comm, cmdline string) bool {
	if comm == "" && cmdline == "" {
		return true
	}
	if strings.Contains(comm, "codex") || strings.Contains(comm, "claude") || comm == "node" {
		if cmdline == "" || strings.Contains(cmdline, "codex") || strings.Contains(cmdline, "claude") {
			return true
		}
	}
	if strings.Contains(cmdline, "codex") || strings.Contains(cmdline, "claude") {
		return true
	}
	return false
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
