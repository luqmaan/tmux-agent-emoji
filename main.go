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
)

var (
	listPanesOutput = func() ([]byte, error) {
		return exec.Command("tmux", "list-panes", "-a",
			"-F", "#{session_name}:#{window_index} #{pane_pid} #{window_active}").Output()
	}
	capturePaneOutput = func(window string) ([]byte, error) {
		return exec.Command("tmux", "capture-pane", "-t", window, "-p").Output()
	}
)

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
)

var spinnerElapsedRE = regexp.MustCompile(`\(\s*\d+[hms](?:\s+\d+[hms])?`)

const unreadIdleThreshold = 3 // cycles (3 * 2s = 6s) before showing 📬 on idle
const unreadAfterWorkCooldown = 20 * time.Second
const claudeIdleCooldown = 15 * time.Second

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
	windowActiveSig  = make(map[string]string)
	windowActiveAt   = make(map[string]time.Time)
)

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

	content := string(out)
	cache[window] = &paneCapture{content: content, ok: true}
	return content, true
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
	for window, s := range summaries {
		now := time.Now()
		rawStatus := s.status
		focused := s.focused
		wasWorking := windowWasWorking[window]
		isWorking := isWorkingStatus(rawStatus)
		seenBefore := windowSeen[window]
		promptSig := ""
		doneSig := ""
		if !isWorking && rawStatus != "" {
			promptSig, doneSig = paneSignals(window, paneCache)
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

		sinceLastWork := unreadAfterWorkCooldown
		if ts, ok := windowLastWorkAt[window]; ok {
			sinceLastWork = now.Sub(ts)
		}

		windowWasWorking[window] = isWorking
		windowSeen[window] = true
		windowPromptSig[window] = promptSig
		windowDoneSig[window] = doneSig

		// Replace 💤 with 📬 only after sustained idle to avoid flicker.
		effectiveStatus := withUnreadMarker(
			rawStatus,
			isWorking,
			isUnread(window),
			windowIdleStreak[window],
			sinceLastWork,
		)
		effectiveStatus = smoothClaudeIdle(effectiveStatus, sinceLastWork)
		effectiveStatus = withDraftMarker(effectiveStatus, promptSig)

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
	for w := range windowActiveSig {
		if !seenWindows[w] {
			delete(windowActiveSig, w)
		}
	}
	for w := range windowActiveAt {
		if !seenWindows[w] {
			delete(windowActiveAt, w)
		}
	}
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
) bool {
	if focused || isWorking || rawStatus == "" {
		return false
	}
	// Codex draft edits in the prompt (unsent user input) should not create unread.
	if strings.HasPrefix(promptSig, "codex:") && hasPromptText(promptSig) && !wasWorking && doneSig == "" {
		return false
	}
	if wasWorking {
		return true
	}
	if !seenBefore {
		// First baseline should stay read for bare prompts, but explicit
		// prompt text ("› Run /review...") indicates immediate attention.
		return hasPromptText(promptSig)
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
		exec.Command("tmux", "rename-window", "-t", window, status).Run()
	} else {
		exec.Command("tmux", "set-option", "-t", window, "automatic-rename", "on").Run()
	}
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

	prefix := "c "
	if agentName == "codex" {
		prefix = "x "
	}

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
			return unknownChildStatus(
				prefix,
				isPaneActive(window, paneCache),
				paneNeedsAttention(window, paneCache),
			)
		}
		return prefix + childStatus
	}

	// Claude and Codex need different idle heuristics:
	// - Claude: active first to suppress prompt-redraw flicker.
	// - Codex: prompt first (original behavior).
	if agentName == "claude" {
		if isPaneActive(window, paneCache) {
			return prefix + "🧠"
		}
		if paneNeedsAttention(window, paneCache) {
			return prefix + "💤"
		}
		return prefix + "💤"
	}
	if paneNeedsAttention(window, paneCache) {
		return prefix + "💤"
	}
	if isPaneActive(window, paneCache) {
		return prefix + "🧠"
	}
	return prefix + "💤"
}

func unknownChildStatus(prefix string, paneActive, needsAttention bool) string {
	// Claude and Codex use different precedence here (see getStatus).
	if strings.HasPrefix(prefix, "c ") {
		if paneActive {
			return prefix + "🧠"
		}
		if needsAttention {
			return prefix + "💤"
		}
		return prefix + "⚙️"
	}

	// Codex legacy behavior: attention takes precedence.
	if needsAttention {
		return prefix + "💤"
	}
	if paneActive {
		return prefix + "🧠"
	}
	return prefix + "⚙️"
}

func paneNeedsAttention(window string, paneCache map[string]*paneCapture) bool {
	content, ok := getPaneContent(window, paneCache)
	if !ok {
		return false
	}
	return classifyPaneNeedsAttention(content)
}

func paneSignals(window string, paneCache map[string]*paneCapture) (promptSig, doneSig string) {
	content, ok := getPaneContent(window, paneCache)
	if !ok {
		return "", ""
	}
	return classifyPaneAttentionSignature(content), classifyPaneCompletionSignature(content)
}

// isPaneActive captures the pane content and checks for activity indicators.
// Uses a grace period to prevent flashing during spinner redraws.
func isPaneActive(window string, paneCache map[string]*paneCapture) bool {
	now := time.Now()
	active := false

	if content, ok := getPaneContent(window, paneCache); ok {
		active = classifyPaneContent(content)
		if active {
			active = !isStaleActiveMarker(window, content, now)
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

func isStaleActiveMarker(window, content string, now time.Time) bool {
	activeSig := classifyPaneActiveSignature(content)
	if activeSig == "" {
		return false
	}
	promptSig := detectPromptSignature(content)
	if promptSig == "" {
		windowActiveSig[window] = activeSig
		windowActiveAt[window] = now
		return false
	}

	prevSig, ok := windowActiveSig[window]
	if !ok || prevSig != activeSig {
		windowActiveSig[window] = activeSig
		windowActiveAt[window] = now
		return false
	}
	startedAt, ok := windowActiveAt[window]
	if !ok {
		windowActiveAt[window] = now
		return false
	}
	return now.Sub(startedAt) >= staleActiveThreshold
}

func clearActiveMarker(window string) {
	delete(windowActiveSig, window)
	delete(windowActiveAt, window)
}

func withUnreadMarker(
	rawStatus string,
	isWorking, unread bool,
	idleStreak int,
	sinceLastWork time.Duration,
) string {
	if isWorking || rawStatus == "" || !unread || !strings.HasSuffix(rawStatus, "💤") {
		return rawStatus
	}

	// Codex uses the original immediate unread marker behavior.
	if strings.HasPrefix(rawStatus, "x ") {
		return strings.TrimSuffix(rawStatus, "💤") + "📬"
	}

	// Claude keeps anti-flicker gating.
	if strings.HasPrefix(rawStatus, "c ") &&
		idleStreak >= unreadIdleThreshold &&
		sinceLastWork >= unreadAfterWorkCooldown {
		return strings.TrimSuffix(rawStatus, "💤") + "📬"
	}
	return rawStatus
}

func smoothClaudeIdle(status string, sinceLastWork time.Duration) string {
	// Claude panes can briefly expose a bare prompt while still in an active run.
	// Hold idle transitions for a short cooldown to prevent 🧠 <-> 💤 flicker.
	if strings.HasPrefix(status, "c ") &&
		strings.HasSuffix(status, "💤") &&
		sinceLastWork < claudeIdleCooldown {
		return "c 🧠"
	}
	return status
}

func withDraftMarker(status, promptSig string) string {
	// Unsent prompt text means "drafting" rather than unread/idle.
	if !strings.HasPrefix(promptSig, "codex:") || !hasPromptText(promptSig) {
		return status
	}
	switch {
	case strings.HasPrefix(status, "x ") && (strings.HasSuffix(status, "💤") || strings.HasSuffix(status, "📬")):
		return "x 📝"
	default:
		return status
	}
}

// classifyPaneContent returns true if the pane content indicates active work.
func classifyPaneContent(content string) bool {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 12; i-- {
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
	return false
}

func hasSpinnerMarker(line string) bool {
	return strings.HasPrefix(line, "· ") ||
		strings.HasPrefix(line, "• ") ||
		strings.HasPrefix(line, "◦ ") ||
		strings.HasPrefix(line, "✢ ") ||
		strings.HasPrefix(line, "✻ ") ||
		strings.HasPrefix(line, "* ")
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
	// Newer Codex spinner lines can be plain gerunds with elapsed timing,
	// e.g. "◦ Investigating ... (1m 08s • esc …)".
	return spinnerElapsedRE.MatchString(line)
}

func classifyPaneActiveSignature(content string) string {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 12; i-- {
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
	if classifyPaneContent(content) {
		return ""
	}
	return detectPromptSignature(content)
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
		if strings.HasPrefix(line, "› ") || line == "›" {
			return "codex:" + line
		}
		if strings.HasPrefix(line, "❯ ") || line == "❯" {
			return "claude:" + line
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
	return strings.HasPrefix(line, "─ Worked for ") ||
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
		"coordinator/cli.ts build", " next build", "npm run build", "pnpm run build", "yarn build", "go build", "cargo build",
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
