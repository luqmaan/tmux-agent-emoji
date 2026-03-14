package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePPIDFromStat(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want int
	}{
		{"simple comm", "123 (bash) S 100 123 123 0 -1 ...", 100},
		{"comm with spaces", "456 (Web Content) S 200 456 456 0 -1 ...", 200},
		{"comm with parens", "789 (foo (bar)) S 300 789 789 0 -1 ...", 300},
		{"empty", "", 0},
		{"truncated", "1 (init) S", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePPIDFromStat(tt.stat)
			if got != tt.want {
				t.Errorf("parsePPIDFromStat(%q) = %d, want %d", tt.stat, got, tt.want)
			}
		})
	}
}

func TestClassifyChildren(t *testing.T) {
	tests := []struct {
		names []string
		want  string
	}{
		{[]string{"gcc"}, "🔨"},
		{[]string{"make", "cc1"}, "🔨"},
		{[]string{"rustc"}, "🔨"},
		{[]string{"jest"}, "🧪"},
		{[]string{"pytest"}, "🧪"},
		{[]string{"npm"}, "📦"},
		{[]string{"pip"}, "📦"},
		{[]string{"git"}, "🔀"},
		{[]string{"curl"}, "🌐"},
		{[]string{"wget"}, "🌐"},
		{[]string{"python3"}, "⚙️"},
		{[]string{"sh"}, "⚙️"},
		{[]string{"rustc", "cargo"}, "🔨"},
		{[]string{"git", "curl"}, "🔀"},
		{[]string{"GCC"}, "🔨"},
		{[]string{"node coordinator/cli.ts build --wait"}, "🔨"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.names, "+"), func(t *testing.T) {
			got := classifyChildren(tt.names)
			if got != tt.want {
				t.Errorf("classifyChildren(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
	}
}

func TestIsAgentLikeProcess(t *testing.T) {
	tests := []struct {
		name    string
		comm    string
		cmdline string
		want    bool
	}{
		{"codex thread", "codex", "", true},
		{"codex binary", "MainThread", "/usr/bin/codex --dangerously-bypass-approvals-and-sandbox", true},
		{"claude binary", "MainThread", "/usr/bin/claude", true},
		{"plain node worker", "node", "node coordinator/cli.ts build --wait", false},
		{"non-agent comm", "psql", "/usr/lib/postgresql/16/bin/psql ...", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAgentLikeProcess(tt.comm, tt.cmdline); got != tt.want {
				t.Errorf("isAgentLikeProcess(%q, %q) = %v, want %v", tt.comm, tt.cmdline, got, tt.want)
			}
		})
	}
}

func TestUnknownChildStatus(t *testing.T) {
	tests := []struct {
		name           string
		policy         agentPolicy
		paneActive     bool
		needsAttention bool
		want           string
	}{
		{"active beats attention", codexPolicy, true, true, "x 🧠"},
		{"active without attention", codexPolicy, true, false, "x 🧠"},
		{"claude active beats attention", claudePolicy, true, true, "c 🧠"},
		{"idle unknown child", codexPolicy, false, false, "x ⚙️"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unknownChildStatus(tt.policy, tt.paneActive, tt.needsAttention)
			if got != tt.want {
				t.Errorf("unknownChildStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPolicyForStatus(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   agentPolicy
	}{
		{name: "codex working", status: "x 🧠", want: codexPolicy},
		{name: "codex draft", status: "x✍️", want: codexPolicy},
		{name: "claude idle", status: "c 💤", want: claudePolicy},
		{name: "claude local agents", status: "c 🤖", want: claudePolicy},
		{name: "claude draft", status: "c✍️", want: claudePolicy},
		{name: "unknown", status: "zsh", want: agentPolicy{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := policyForStatus(tt.status); got != tt.want {
				t.Errorf("policyForStatus(%q) = %+v, want %+v", tt.status, got, tt.want)
			}
		})
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("hello world", "hello") {
		t.Error("should contain hello")
	}
	if containsAny("hello world", "foo", "bar") {
		t.Error("should not contain foo or bar")
	}
	if !containsAny("hello world", "foo", "world") {
		t.Error("should contain world")
	}
}

func TestTmuxArgs(t *testing.T) {
	t.Run("without socket override", func(t *testing.T) {
		t.Setenv(tmuxSocketEnv, "")
		got := tmuxArgs("list-panes", "-a")
		want := []string{"list-panes", "-a"}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("tmuxArgs() = %v, want %v", got, want)
		}
	})

	t.Run("with socket override", func(t *testing.T) {
		t.Setenv(tmuxSocketEnv, "/tmp/tmux-test.sock")
		got := tmuxArgs("list-panes", "-a")
		want := []string{"-S", "/tmp/tmux-test.sock", "list-panes", "-a"}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("tmuxArgs() = %v, want %v", got, want)
		}
	})
}

func TestCollectDescendants(t *testing.T) {
	childMap := map[int][]int{
		1: {2, 3}, 2: {4}, 3: {5, 6}, 6: {7},
	}
	got := collectDescendants(1, childMap)
	want := map[int]bool{2: true, 3: true, 4: true, 5: true, 6: true, 7: true}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d", len(got), len(want))
	}
	for _, pid := range got {
		if !want[pid] {
			t.Errorf("unexpected pid %d", pid)
		}
	}
}

func TestCollectDescendants_Empty(t *testing.T) {
	got := collectDescendants(1, map[int][]int{1: {}})
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestGetStatus_NoAgent(t *testing.T) {
	myPID := os.Getpid()
	status := getStatus("fake:0", myPID, map[int][]int{myPID: {}}, map[string]*paneCapture{})
	if status != "" {
		t.Errorf("expected empty status, got %q", status)
	}
}

func TestBuildChildMap_ContainsSelf(t *testing.T) {
	m := buildChildMap()
	found := false
	for _, c := range m[os.Getppid()] {
		if c == os.Getpid() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("PID %d not found under PPID %d", os.Getpid(), os.Getppid())
	}
}

func TestReadComm_Self(t *testing.T) {
	if readComm(os.Getpid()) == "" {
		t.Error("readComm for self should not be empty")
	}
}

func TestReadCmdline_Self(t *testing.T) {
	if readCmdline(os.Getpid()) == "" {
		t.Error("readCmdline for self should not be empty")
	}
}

func TestReadComm_InvalidPID(t *testing.T) {
	if readComm(999999999) != "" {
		t.Error("should be empty for invalid PID")
	}
}

func TestReadCmdline_InvalidPID(t *testing.T) {
	if readCmdline(999999999) != "" {
		t.Error("should be empty for invalid PID")
	}
}

func TestFindAgent_NoChildren(t *testing.T) {
	pid, name := findAgent(100, map[int][]int{100: {}})
	if pid != 0 || name != "" {
		t.Errorf("expected no agent, got pid=%d name=%q", pid, name)
	}
}

func TestFindAgent_DoesNotExceedGrandchildren(t *testing.T) {
	childMap := map[int][]int{100: {200}, 200: {300}, 300: {400}}
	pid, _ := findAgent(100, childMap)
	if pid != 0 {
		t.Error("should not find agent at great-grandchild level")
	}
}

func TestReadPPID_Self(t *testing.T) {
	if readPPID(os.Getpid()) != os.Getppid() {
		t.Errorf("readPPID(self) = %d, want %d", readPPID(os.Getpid()), os.Getppid())
	}
}

func TestListPanes_NoCrash(t *testing.T) {
	_ = listPanes()
}

func TestListPanes_ParsesOutput(t *testing.T) {
	orig := listPanesOutput
	defer func() { listPanesOutput = orig }()

	listPanesOutput = func() ([]byte, error) {
		return []byte(
			"s:1 123 1\n" +
				"s:2 234 0\n" +
				"badline\n" +
				"s:3 nope 1\n",
		), nil
	}

	got := listPanes()
	if len(got) != 2 {
		t.Fatalf("expected 2 parsed panes, got %d (%v)", len(got), got)
	}
	if got[0].window != "s:1" || got[0].pid != 123 || !got[0].focused {
		t.Errorf("unexpected first pane: %+v", got[0])
	}
	if got[1].window != "s:2" || got[1].pid != 234 || got[1].focused {
		t.Errorf("unexpected second pane: %+v", got[1])
	}
}

func TestGetPaneContent_CachesSuccess(t *testing.T) {
	orig := capturePaneOutput
	defer func() { capturePaneOutput = orig }()

	calls := 0
	capturePaneOutput = func(window string) ([]byte, error) {
		calls++
		return []byte("hello"), nil
	}

	cache := map[string]*paneCapture{}
	content, ok := getPaneContent("w:1", cache)
	if !ok || content != "hello" {
		t.Fatalf("expected first call success, got ok=%v content=%q", ok, content)
	}
	content, ok = getPaneContent("w:1", cache)
	if !ok || content != "hello" {
		t.Fatalf("expected cached success, got ok=%v content=%q", ok, content)
	}
	if calls != 1 {
		t.Fatalf("expected capturePaneOutput called once, got %d", calls)
	}
}

func TestGetPaneContent_CachesFailure(t *testing.T) {
	orig := capturePaneOutput
	defer func() { capturePaneOutput = orig }()

	calls := 0
	capturePaneOutput = func(window string) ([]byte, error) {
		calls++
		return nil, errors.New("boom")
	}

	cache := map[string]*paneCapture{}
	content, ok := getPaneContent("w:2", cache)
	if ok || content != "" {
		t.Fatalf("expected first call failure, got ok=%v content=%q", ok, content)
	}
	content, ok = getPaneContent("w:2", cache)
	if ok || content != "" {
		t.Fatalf("expected cached failure, got ok=%v content=%q", ok, content)
	}
	if calls != 1 {
		t.Fatalf("expected capturePaneOutput called once, got %d", calls)
	}
}

func TestReadCmdline_NullBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cmdline")
	os.WriteFile(path, []byte("/usr/bin/node\x00/path/to/claude\x00--flag\x00"), 0644)
	data, _ := os.ReadFile(path)
	result := strings.ReplaceAll(string(data), "\x00", " ")
	if !strings.Contains(result, "claude") {
		t.Errorf("should contain claude, got %q", result)
	}
}

// --- classifyPaneContent tests ---

func TestClassifyPaneContent_Active(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"esc to interrupt", "some output\n  (esc to interrupt)\n❯ \n"},
		{"claude thinking", "· Thinking… (5s · esc to interrupt)\n❯ \n"},
		{"codex planning", "• Planning try removal patch (5m 42s • esc to interrupt)\n› \n"},
		{"spinner no esc", "✢ Transfiguring… (thought for 6s)\n❯ \n"},
		{"brewing no esc", "· Brewing… (2s)\n❯ \n"},
		{"leavening", "· Leavening… (54s · ↑ 1.0k tokens · thought for 28s)\n❯ \n"},
		{"unknown future verb", "✻ Zymurgying… (3s)\n❯ \n"},
		{"three dots", "· Pondering... (1s)\n❯ \n"},
		{"accomplishing", "· Accomplishing… (1m 13s · ↓ 1.3k tokens · thought for 20s)\n❯ \n"},
		{"bare spinner no parens", "* Perusing…\n\n──────\n❯ \n"},
		{"bare spinner three dots", "· Thinking...\n❯ \n"},
		{"claude compacting marker", "● Compacting conversation context… (2m 01s)\n❯ \n"},
		{"spinner elapsed timer without ellipsis", "◦ Investigating window process states (1m 08s • esc …)\n› \n"},
		{
			"active spinner above prompt text",
			"• Implementing normalization, filtering, and selection logic (2m 23s • esc to interrupt)\n" +
				"\n" +
				"› Run /review on my current changes\n" +
				"\n" +
				"  gpt-5.3-codex xhigh · 58% left · ~/content-magic-weaver\n",
		},
		{
			"star spinner with thought elapsed",
			"· Fixing wrong store logos… (thought for 8s)\n" +
				"  ⎿  ◼ Fix wrong store logos\n" +
				"     ◻ Fix Inness boutique store logo\n" +
				"\n" +
				"─────────────────\n" +
				"❯ \n" +
				"─────────────────\n" +
				"  🟢 41%\n" +
				"  ⏵⏵ bypass permissions on\n",
		},
		{
			"star6 spinner at top with task list",
			"✶ Fixing wrong store logos… (8m 52s · thinking)\n" +
				"  ⎿  ◼ Fix wrong store logos\n" +
				"     ◻ Fix other task\n" +
				"      … +2 pending\n" +
				"\n" +
				"─────────────────\n" +
				"❯ \n" +
				"─────────────────\n" +
				"  🟢 41%\n" +
				"  ⏵⏵ bypass permissions on\n",
		},
		{
			"agent subprocess marker",
			"● Agent(Download correct store logos)\n" +
				"  ⎿  ◼ Fix wrong store logos\n" +
				"\n" +
				"─────────────────\n" +
				"❯ \n" +
				"─────────────────\n" +
				"  🟢 41%\n" +
				"  ⏵⏵ bypass permissions on\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !classifyPaneContent(tt.content) {
				t.Errorf("expected active for %q", tt.name)
			}
		})
	}
}

func TestClassifyPaneContent_Idle(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"claude idle", "output\n\n❯ \n──────\n  🟢 19%\n  ⏵⏵ bypass permissions on\n"},
		{"claude worked", "✻ Worked for 1m 51s\n❯ \n"},
		{"codex idle", "Done.\n\n› Explain this codebase\n\n  gpt-5.3-codex · 87% left\n"},
		{"codex worked", "─ Worked for 1m 51s ──────\n• Deployed.\n› \n"},
		{"codex cogitated", "✻ Cogitated for 1m 27s\n❯ \n"},
		{"filled circle command output", "● Bash(source ./env)\n❯ \n"},
		{"prose contains ing dots", "Discussion summary...\nI am discussing...\n› Explain this codebase\n"},
		{"empty", ""},
		{"plain shell", "$ ls\nfile1\n$ \n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if classifyPaneContent(tt.content) {
				t.Errorf("expected idle for %q", tt.name)
			}
		})
	}
}

func TestClassifyPaneBackgroundAgents(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "local agents footer near prompt",
			content: "All set.\n" +
				"─────────────────\n" +
				"❯ \n" +
				"─────────────────\n" +
				"  🟢 41%\n" +
				"  ⏵⏵ bypass permissions on · 8 local agents\n",
			want: true,
		},
		{
			name: "background tasks summary near bottom",
			content: "Reports incoming.\n" +
				"● Agent \"Trust signals: shoepalace\" completed\n" +
				"✻ Cogitated for 4m 28s · 10 background tasks still running (↓ to manage)\n" +
				"● Agent \"Trust signals: kithnyc\" completed\n" +
				"❯ \n" +
				"  🟢 7%\n" +
				"  ⏵⏵ bypass permissions on\n",
			want: true,
		},
		{
			name: "stale local agents mention scrolled away",
			content: "⏵⏵ bypass permissions on · 8 local agents\n" +
				"line 1\n" +
				"line 2\n" +
				"line 3\n" +
				"line 4\n" +
				"line 5\n" +
				"line 6\n" +
				"line 7\n" +
				"line 8\n" +
				"line 9\n" +
				"line 10\n" +
				"line 11\n" +
				"line 12\n",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPaneBackgroundAgents(tt.content); got != tt.want {
				t.Errorf("classifyPaneBackgroundAgents() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyPaneNeedsAttention(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "codex waiting at prompt",
			content: "Done.\n\n› Run /review on my current changes\n\n" +
				"  gpt-5.3-codex · 87% left\n",
			want: true,
		},
		{
			name: "claude waiting at prompt",
			content: "All set.\n\n❯ \n──────\n" +
				"  🟢 19%\n",
			want: true,
		},
		{
			name:    "active spinner is not attention",
			content: "· Thinking… (5s · esc to interrupt)\n❯ \n",
			want:    false,
		},
		{
			name: "local agents are not attention",
			content: "All set.\n" +
				"─────────────────\n" +
				"❯ \n" +
				"─────────────────\n" +
				"  🟢 41%\n" +
				"  ⏵⏵ bypass permissions on · 8 local agents\n",
			want: false,
		},
		{
			name:    "plain output",
			content: "$ ls\nfile1\n$ \n",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPaneNeedsAttention(tt.content); got != tt.want {
				t.Errorf("classifyPaneNeedsAttention(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestClassifyPaneAttentionSignature(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "codex prompt with text",
			content: "Done.\n\n› Explain this codebase\n" +
				"  gpt-5.3-codex · 87% left\n",
			want: "codex:› Explain this codebase",
		},
		{
			name:    "claude bare prompt",
			content: "All set.\n\n❯ \n",
			want:    "claude:❯",
		},
		{
			name:    "active spinner has no prompt signature",
			content: "· Thinking… (5s · esc to interrupt)\n❯ \n",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPaneAttentionSignature(tt.content); got != tt.want {
				t.Errorf("classifyPaneAttentionSignature(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestClassifyPaneActiveSignature(t *testing.T) {
	content := "Done.\n\n◦ Planning broad tests and monitoring (1m 03s • esc to interrupt)\n› Find and fix a bug in @filename\n"
	got := classifyPaneActiveSignature(content)
	want := "◦ Planning broad tests and monitoring (1m 03s • esc to interrupt)"
	if got != want {
		t.Errorf("classifyPaneActiveSignature() = %q, want %q", got, want)
	}
}

func TestDetectPromptSignature(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "codex prompt",
			content: "Done.\n\n› Summarize recent commits\n\n  gpt-5.3-codex xhigh · 45% left\n",
			want:    "codex:› Summarize recent commits",
		},
		{
			name:    "claude prompt with nbsp separator",
			content: "All set.\n\n❯\u00a0draft a reply\n",
			want:    "claude:❯\u00a0draft a reply",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectPromptSignature(tt.content); got != tt.want {
				t.Errorf("detectPromptSignature() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectMeaningfulPromptSignature(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "codex typed prompt stays explicit",
			content: "Done.\n\n› fix this bug",
			want:    "codex:› fix this bug",
		},
		{
			name:    "codex dim suggestion becomes bare prompt",
			content: "\x1b[1m›\x1b[0m\x1b[48;5;237m \x1b[2mExplain this codebase\x1b[0m",
			want:    "codex:›",
		},
		{
			name: "claude ghost suggestion becomes bare prompt",
			content: "All set.\n" +
				"────────────────\n" +
				"❯\u00a0\x1b[7mo\x1b[0;2mk now let's start implementing this\x1b[0m\n" +
				"────────────────\n" +
				"🟢 43%\n" +
				"⏵⏵ bypass permissions on (shift+tab to cycle)\n",
			want: "claude:❯",
		},
		{
			name: "claude typed prompt with footer stays explicit",
			content: "All set.\n" +
				"────────────────\n" +
				"❯\u00a0draft this\n" +
				"────────────────\n" +
				"🟢 35%\n" +
				"⏵⏵ bypass permissions on (shift+tab to cycle)\n",
			want: "claude:❯\u00a0draft this",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectMeaningfulPromptSignature(tt.content); got != tt.want {
				t.Errorf("detectMeaningfulPromptSignature() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsStaleActiveMarker(t *testing.T) {
	window := "test:stale-active"
	content := "◦ Planning broad tests and monitoring (1m 03s • esc to interrupt)\n› Find and fix a bug in @filename\n"

	delete(windowActiveSig, window)
	delete(windowActiveAt, window)
	defer func() {
		delete(windowActiveSig, window)
		delete(windowActiveAt, window)
	}()

	now := time.Now()
	if stale := isStaleActiveMarker(window, content, now); stale {
		t.Error("first seen active marker should not be stale")
	}
	if stale := isStaleActiveMarker(window, content, now.Add(staleActiveThreshold+time.Second)); !stale {
		t.Error("unchanged active marker with prompt should become stale")
	}
}

func TestClassifyPaneCompletionSignature(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "codex worked marker",
			content: "─ Worked for 2m 21s ─\n• Summary\n› Next task\n",
			want:    "─ Worked for 2m 21s ─",
		},
		{
			name:    "claude worked marker",
			content: "✻ Worked for 2m 21s\n\n❯ \n",
			want:    "✻ Worked for 2m 21s",
		},
		{
			name:    "done line",
			content: "Done.\n\n› Explain this codebase\n",
			want:    "Done.",
		},
		{
			name:    "no completion marker",
			content: "Random output\n› prompt\n",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPaneCompletionSignature(tt.content); got != tt.want {
				t.Errorf("classifyPaneCompletionSignature(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// --- Debounce / grace period tests ---

func TestIsPaneActive_GracePeriod(t *testing.T) {
	window := "test:99"

	// Seed as recently active
	lastActiveMu.Lock()
	lastActive[window] = time.Now()
	lastActiveMu.Unlock()

	// isPaneActive calls tmux which won't have this window,
	// so capture-pane fails → content check returns false.
	// But grace period should still return true.
	result := isPaneActive(window, map[string]*paneCapture{})
	if !result {
		t.Error("should return true during grace period even if capture fails")
	}

	// Clean up
	lastActiveMu.Lock()
	delete(lastActive, window)
	lastActiveMu.Unlock()
}

func TestIsPaneActive_GraceExpired(t *testing.T) {
	window := "test:98"

	// Seed as active long ago (past grace period)
	lastActiveMu.Lock()
	lastActive[window] = time.Now().Add(-activeGrace - time.Second)
	lastActiveMu.Unlock()

	result := isPaneActive(window, map[string]*paneCapture{})
	if result {
		t.Error("should return false after grace period expires")
	}

	// Clean up in case
	lastActiveMu.Lock()
	delete(lastActive, window)
	lastActiveMu.Unlock()
}

func TestIsPaneActive_NoHistory(t *testing.T) {
	window := "test:97"

	// No history, capture will fail → should be false
	lastActiveMu.Lock()
	delete(lastActive, window)
	lastActiveMu.Unlock()

	result := isPaneActive(window, map[string]*paneCapture{})
	if result {
		t.Error("should return false with no history and no content")
	}
}

// --- Unread tracking tests ---

func TestIsWorkingStatus(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"🧠", true},
		{"🔨", true},
		{"⚙️", true},
		{"x 🧠", true},
		{"x 🔨", true},
		{"c 🧠", true},
		{"💤", false},
		{"c 💤", false},
		{"x 💤", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := isWorkingStatus(tt.status); got != tt.want {
				t.Errorf("isWorkingStatus(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestStatusPriority(t *testing.T) {
	if statusPriority("🧠") <= statusPriority("💤") {
		t.Error("working should have higher priority than idle")
	}
	if statusPriority("💤") <= statusPriority("") {
		t.Error("idle should have higher priority than empty")
	}
}

func TestUnreadMarkAndClear(t *testing.T) {
	window := "test:unread1"

	// Clean state
	statusStateMu.Lock()
	delete(statusState, window)
	statusStateMu.Unlock()

	if isUnread(window) {
		t.Error("should not be unread initially")
	}

	markUnread(window)
	if !isUnread(window) {
		t.Error("should be unread after marking")
	}

	clearUnread(window)
	if isUnread(window) {
		t.Error("should not be unread after clearing")
	}

	// Clean up
	statusStateMu.Lock()
	delete(statusState, window)
	statusStateMu.Unlock()
}

func TestUnreadReplacesIdle(t *testing.T) {
	// When unread, 💤 should become 📬
	window := "test:unread2"

	statusStateMu.Lock()
	delete(statusState, window)
	statusStateMu.Unlock()

	markUnread(window)

	status := "💤"
	if isUnread(window) && strings.HasSuffix(status, "💤") {
		status = strings.TrimSuffix(status, "💤") + "📬"
	}
	if status != "📬" {
		t.Errorf("expected 📬, got %q", status)
	}

	// Codex variant
	status = "x 💤"
	if isUnread(window) && strings.HasSuffix(status, "💤") {
		status = strings.TrimSuffix(status, "💤") + "📬"
	}
	if status != "x 📬" {
		t.Errorf("expected x 📬, got %q", status)
	}

	// Clean up
	statusStateMu.Lock()
	delete(statusState, window)
	statusStateMu.Unlock()
}

func TestWithUnreadMarkerThreshold(t *testing.T) {
	tests := []struct {
		name       string
		rawStatus  string
		isWorking  bool
		unread     bool
		idleStreak int
		want       string
	}{
		{
			name:       "below threshold stays idle",
			rawStatus:  "c 💤",
			unread:     true,
			idleStreak: unreadIdleThreshold - 1,
			want:       "c 💤",
		},
		{
			name:       "at threshold becomes mailbox",
			rawStatus:  "c 💤",
			unread:     true,
			idleStreak: unreadIdleThreshold,
			want:       "c 📬",
		},
		{
			name:       "working never becomes mailbox",
			rawStatus:  "c 🧠",
			isWorking:  true,
			unread:     true,
			idleStreak: unreadIdleThreshold + 5,
			want:       "c 🧠",
		},
		{
			name:       "non-idle suffix unchanged",
			rawStatus:  "c 🔨",
			unread:     true,
			idleStreak: unreadIdleThreshold + 5,
			want:       "c 🔨",
		},
		{
			name:       "codex unread is immediate (legacy behavior)",
			rawStatus:  "x 💤",
			unread:     true,
			idleStreak: 0,
			want:       "x 📬",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withUnreadMarker(tt.rawStatus, tt.isWorking, tt.unread, tt.idleStreak); got != tt.want {
				t.Errorf("withUnreadMarker() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClaudeUnreadAppearsAsSoonAsIdleCooldownEnds(t *testing.T) {
	status := withDraftMarker("c 💤", "")
	status = smoothIdleStatus(status, claudeIdleCooldown+time.Second)
	status = withUnreadMarker(status, isWorkingStatus(status), true, unreadIdleThreshold)

	if status != "c 📬" {
		t.Errorf("expected c 📬 once idle cooldown ends, got %q", status)
	}
}

func TestClaudeUnreadStaysWorkingDuringIdleCooldown(t *testing.T) {
	status := withDraftMarker("c 💤", "")
	status = smoothIdleStatus(status, claudeIdleCooldown-time.Second)
	status = withUnreadMarker(status, isWorkingStatus(status), true, unreadIdleThreshold)

	if status != "c 🧠" {
		t.Errorf("expected c 🧠 during idle cooldown, got %q", status)
	}
}

func TestResolveDisplayStatus(t *testing.T) {
	tests := []struct {
		name          string
		rawStatus     string
		draftSig      string
		unread        bool
		idleStreak    int
		sinceLastWork time.Duration
		want          string
	}{
		{
			name:          "claude draft beats unread",
			rawStatus:     "c 💤",
			draftSig:      "claude:❯ revise this",
			unread:        true,
			idleStreak:    unreadIdleThreshold + 2,
			sinceLastWork: claudeIdleCooldown + time.Second,
			want:          "c✍️",
		},
		{
			name:          "claude stays active during idle cooldown",
			rawStatus:     "c 💤",
			unread:        true,
			idleStreak:    unreadIdleThreshold + 2,
			sinceLastWork: claudeIdleCooldown - time.Second,
			want:          "c 🧠",
		},
		{
			name:          "claude unread after cooldown and stable idle",
			rawStatus:     "c 💤",
			unread:        true,
			idleStreak:    unreadIdleThreshold,
			sinceLastWork: claudeIdleCooldown + time.Second,
			want:          "c 📬",
		},
		{
			name:          "codex unread remains immediate",
			rawStatus:     "x 💤",
			unread:        true,
			idleStreak:    1,
			sinceLastWork: time.Second,
			want:          "x 📬",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveDisplayStatus(tt.rawStatus, tt.draftSig, tt.unread, tt.idleStreak, tt.sinceLastWork); got != tt.want {
				t.Errorf("resolveDisplayStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSmoothIdleStatus(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		sinceWork time.Duration
		want      string
	}{
		{
			name:      "claude idle held as active within cooldown",
			status:    "c 💤",
			sinceWork: claudeIdleCooldown - time.Second,
			want:      "c 🧠",
		},
		{
			name:      "claude idle allowed after cooldown",
			status:    "c 💤",
			sinceWork: claudeIdleCooldown + time.Second,
			want:      "c 💤",
		},
		{
			name:      "codex idle unaffected",
			status:    "x 💤",
			sinceWork: claudeIdleCooldown - time.Second,
			want:      "x 💤",
		},
		{
			name:      "non-idle status unchanged",
			status:    "c 🔨",
			sinceWork: claudeIdleCooldown - time.Second,
			want:      "c 🔨",
		},
		{
			name:      "draft marker not overridden by cooldown",
			status:    "c✍️",
			sinceWork: claudeIdleCooldown - time.Second,
			want:      "c✍️",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := smoothIdleStatus(tt.status, tt.sinceWork); got != tt.want {
				t.Errorf("smoothIdleStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func readPaneFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "panes", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return string(data)
}

func TestPaneFixtures(t *testing.T) {
	t.Run("claude active fixture", func(t *testing.T) {
		content := readPaneFixture(t, "claude-active.txt")
		if !classifyPaneContent(content) {
			t.Fatal("expected Claude fixture to classify as active")
		}
		if sig := classifyPaneActiveSignature(content); sig == "" {
			t.Fatal("expected Claude fixture to produce an active signature")
		}
	})

	t.Run("codex idle fixture", func(t *testing.T) {
		content := readPaneFixture(t, "codex-idle.txt")
		if classifyPaneContent(content) {
			t.Fatal("expected Codex fixture to classify as idle")
		}
		if !classifyPaneNeedsAttention(content) {
			t.Fatal("expected Codex fixture to need attention")
		}
		if sig := classifyPaneAttentionSignature(content); sig != "codex:› Explain this codebase" {
			t.Fatalf("unexpected Codex prompt signature %q", sig)
		}
	})

	t.Run("claude draft fixture", func(t *testing.T) {
		content := readPaneFixture(t, "claude-draft.txt")
		if sig := detectLiveDraftSignature(content); sig != "claude:❯ draft this message" {
			t.Fatalf("unexpected Claude draft signature %q", sig)
		}
	})
}

func TestWithDraftMarker(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		promptSig string
		want      string
	}{
		{
			name:      "codex idle with typed prompt becomes drafting",
			status:    "x 💤",
			promptSig: "codex:› investigate this",
			want:      "x✍️",
		},
		{
			name:      "codex unread with typed prompt becomes drafting",
			status:    "x 📬",
			promptSig: "codex:› investigate this",
			want:      "x✍️",
		},
		{
			name:      "claude idle with typed prompt becomes drafting",
			status:    "c 💤",
			promptSig: "claude:❯ investigate this",
			want:      "c✍️",
		},
		{
			name:      "claude unread with typed prompt becomes drafting",
			status:    "c 📬",
			promptSig: "claude:❯ investigate this",
			want:      "c✍️",
		},
		{
			name:      "codex bare prompt does not become drafting",
			status:    "x 💤",
			promptSig: "codex:›",
			want:      "x 💤",
		},
		{
			name:      "claude active status unchanged",
			status:    "c 🧠",
			promptSig: "claude:❯ test",
			want:      "c 🧠",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withDraftMarker(tt.status, tt.promptSig); got != tt.want {
				t.Errorf("withDraftMarker() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectLiveDraftSignature(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "bottom codex prompt with text is draft",
			content: "Done.\n\n› draft this message",
			want:    "codex:› draft this message",
		},
		{
			name:    "bare prompt is not draft",
			content: "Done.\n\n›",
			want:    "",
		},
		{
			name:    "prompt text not at bottom is not draft",
			content: "› old prompt text\nSome other output",
			want:    "",
		},
		{
			name:    "bottom claude prompt with text is draft",
			content: "All set.\n\n❯ draft this",
			want:    "claude:❯ draft this",
		},
		{
			name:    "bottom claude prompt with nbsp is draft",
			content: "All set.\n\n❯\u00a0draft this",
			want:    "claude:❯\u00a0draft this",
		},
		{
			name: "claude prompt with footer lines below is draft",
			content: "All set.\n" +
				"────────────────\n" +
				"❯\u00a0draft this\n" +
				"────────────────\n" +
				"🟢 35%\n" +
				"⏵⏵ bypass permissions on (shift+tab to cycle)\n",
			want: "claude:❯\u00a0draft this",
		},
		{
			name: "claude ghost suggestion line is not draft",
			content: "All set.\n" +
				"────────────────\n" +
				"❯\u00a0\x1b[7mo\x1b[0;2mk now let's start implementing this\x1b[0m\n" +
				"────────────────\n" +
				"🟢 43%\n" +
				"⏵⏵ bypass permissions on (shift+tab to cycle)\n",
			want: "",
		},
		{
			name:    "codex dim suggestion line is not draft",
			content: "\x1b[1m›\x1b[0m\x1b[48;5;237m \x1b[2mExplain this codebase\x1b[0m",
			want:    "",
		},
		{
			name: "prompt with real output below is not draft",
			content: "All set.\n\n❯ draft this\n" +
				"Task finished.\n",
			want: "",
		},
		{
			name:    "bare claude prompt is not draft",
			content: "All set.\n\n❯",
			want:    "",
		},
		{
			name: "multi-line wrapped draft with footer chrome is draft",
			content: "  ⎿  Interrupted · What should Claude do instead?\n\n" +
				"────────────────────────\n" +
				"❯ n make aure tou covwr all the todo list items. if\n" +
				"  rhere are bugs add tests for them. and make sure\n" +
				"  they dont happen again. if there are data issues\n" +
				"  make sure yoi baxkfill\n" +
				"────────────────────────\n" +
				"💬\n" +
				"⏵⏵ bypass permissions on (shift+tab to cycle)\n",
			want: "claude:❯ n make aure tou covwr all the todo list items. if",
		},
		{
			name: "multi-line wrapped draft without mode indicator is draft",
			content: "────────────────────────\n" +
				"❯ fix the bug in login flow and add\n" +
				"  a regression test for it\n" +
				"────────────────────────\n" +
				"⏵⏵ bypass permissions on\n",
			want: "claude:❯ fix the bug in login flow and add",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectLiveDraftSignature(tt.content); got != tt.want {
				t.Errorf("detectLiveDraftSignature() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShouldMarkUnread(t *testing.T) {
	tests := []struct {
		name          string
		wasWorking    bool
		focused       bool
		isWorking     bool
		rawStatus     string
		seenBefore    bool
		promptSig     string
		prevPromptSig string
		doneSig       string
		prevDoneSig   string
		isLiveDraft   bool
		want          bool
	}{
		{
			name:       "working to idle unfocused",
			wasWorking: true, rawStatus: "x 💤", want: true,
		},
		{
			name:          "codex draft prompt change stays read",
			seenBefore:    true,
			rawStatus:     "x 💤",
			prevPromptSig: "codex:› old",
			promptSig:     "codex:› new draft text",
			isLiveDraft:   true,
			want:          false,
		},
		{
			name:          "claude draft prompt change stays read",
			seenBefore:    true,
			rawStatus:     "c 💤",
			prevPromptSig: "claude:❯ old",
			promptSig:     "claude:❯ new draft text",
			isLiveDraft:   true,
			want:          false,
		},
		{
			name:        "new completion signature after baseline",
			seenBefore:  true,
			rawStatus:   "x 💤",
			prevDoneSig: "─ Worked for 1m 00s ─",
			doneSig:     "─ Worked for 1m 10s ─",
			want:        true,
		},
		{
			name:        "first baseline codex draft stays read",
			seenBefore:  false,
			rawStatus:   "x 💤",
			promptSig:   "codex:› Explain this codebase",
			isLiveDraft: true,
			want:        false,
		},
		{
			name:       "working to idle codex with prompt text still marks unread",
			wasWorking: true,
			rawStatus:  "x 💤",
			promptSig:  "codex:› continue",
			want:       true,
		},
		{
			name:       "first baseline bare prompt stays read",
			seenBefore: false,
			rawStatus:  "x 💤",
			promptSig:  "codex:›",
			want:       false,
		},
		{
			name:       "first baseline bare claude prompt stays read",
			seenBefore: false,
			rawStatus:  "c 💤",
			promptSig:  "claude:❯",
			want:       false,
		},
		{
			name:          "unchanged signatures stay read",
			seenBefore:    true,
			rawStatus:     "x 💤",
			promptSig:     "codex:› Explain this codebase",
			prevPromptSig: "codex:› Explain this codebase",
			want:          false,
		},
		{
			name:      "focused clears attention",
			focused:   true,
			rawStatus: "x 💤",
			doneSig:   "Done.",
			want:      false,
		},
		{
			name:      "still working",
			isWorking: true,
			rawStatus: "x 🧠",
			want:      false,
		},
		{
			name:      "empty status",
			rawStatus: "",
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldMarkUnread(
				tt.wasWorking,
				tt.focused,
				tt.isWorking,
				tt.rawStatus,
				tt.seenBefore,
				tt.promptSig,
				tt.prevPromptSig,
				tt.doneSig,
				tt.prevDoneSig,
				tt.isLiveDraft,
			)
			if got != tt.want {
				t.Errorf("shouldMarkUnread() = %v, want %v", got, tt.want)
			}
		})
	}
}

type testWindowCycleState struct {
	wasWorking bool
	seenBefore bool
	promptSig  string
	doneSig    string
	idleStreak int
	lastWorkAt time.Time
	unread     bool
}

func (s *testWindowCycleState) step(
	now time.Time,
	rawStatus string,
	focused bool,
	promptSig, doneSig, liveDraftSig string,
) string {
	isWorking := isWorkingStatus(rawStatus)
	if shouldMarkUnread(
		s.wasWorking,
		focused,
		isWorking,
		rawStatus,
		s.seenBefore,
		promptSig,
		s.promptSig,
		doneSig,
		s.doneSig,
		liveDraftSig != "",
	) {
		s.unread = true
	}
	if focused || isWorking {
		s.unread = false
	}
	if isWorking {
		s.lastWorkAt = now
	}
	if !isWorking && rawStatus != "" && strings.HasSuffix(rawStatus, "💤") {
		s.idleStreak++
	} else {
		s.idleStreak = 0
	}

	sinceLastWork := claudeIdleCooldown
	if !s.lastWorkAt.IsZero() {
		sinceLastWork = now.Sub(s.lastWorkAt)
	}

	s.wasWorking = isWorking
	s.seenBefore = true
	s.promptSig = promptSig
	s.doneSig = doneSig

	return resolveDisplayStatus(rawStatus, liveDraftSig, s.unread, s.idleStreak, sinceLastWork)
}

func TestWindowStatusSequence_ClaudeCompletionUnreadAndFocusClear(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	var state testWindowCycleState

	if got := state.step(base, "c 🧠", false, "", "", ""); got != "c 🧠" {
		t.Fatalf("step 1 = %q, want c 🧠", got)
	}
	if state.unread {
		t.Fatal("unread should stay false while Claude is working")
	}

	doneSig := "All clear."
	promptSig := "claude:❯"
	for i, tc := range []struct {
		after time.Duration
		want  string
	}{
		{after: 5 * time.Second, want: "c 🧠"},
		{after: claudeIdleCooldown + time.Second, want: "c 💤"},
		{after: claudeIdleCooldown + 3*time.Second, want: "c 📬"},
	} {
		now := base.Add(tc.after)
		got := state.step(now, "c 💤", false, promptSig, doneSig, "")
		if got != tc.want {
			t.Fatalf("idle step %d = %q, want %q", i+1, got, tc.want)
		}
	}
	if !state.unread {
		t.Fatal("Claude completion should leave the window unread while unfocused")
	}

	if got := state.step(base.Add(30*time.Second), "c 💤", true, promptSig, doneSig, ""); got != "c 💤" {
		t.Fatalf("focused step = %q, want c 💤", got)
	}
	if state.unread {
		t.Fatal("focus should clear unread")
	}
}

func TestWindowStatusSequence_CodexUnreadImmediate(t *testing.T) {
	base := time.Unix(1_700_000_100, 0)
	var state testWindowCycleState

	if got := state.step(base, "x 🧠", false, "", "", ""); got != "x 🧠" {
		t.Fatalf("step 1 = %q, want x 🧠", got)
	}

	got := state.step(base.Add(2*time.Second), "x 💤", false, "codex:›", "Done.", "")
	if got != "x 📬" {
		t.Fatalf("step 2 = %q, want x 📬", got)
	}
	if !state.unread {
		t.Fatal("Codex completion should mark unread immediately")
	}
}

func TestWindowStatusSequence_CodexGhostSuggestionStaysRead(t *testing.T) {
	base := time.Unix(1_700_000_200, 0)
	var state testWindowCycleState

	ghostContent := "\x1b[1m›\x1b[0m\x1b[48;5;237m \x1b[2mWrite tests for @filename\x1b[0m"
	promptSig := detectMeaningfulPromptSignature(ghostContent)

	got := state.step(base, "x 💤", false, promptSig, "", "")
	if got != "x 💤" {
		t.Fatalf("step 1 = %q, want x 💤", got)
	}
	if state.unread {
		t.Fatal("ghost suggestion should not mark unread")
	}
}

// --- Benchmarks ---

func BenchmarkBuildChildMap(b *testing.B) {
	for i := 0; i < b.N; i++ {
		buildChildMap()
	}
}

func BenchmarkClassifyPaneContent(b *testing.B) {
	content := "· Brewing… (1m 20s · ↓ 1.8k tokens)\n  (esc to interrupt)\n❯ \n"
	for i := 0; i < b.N; i++ {
		classifyPaneContent(content)
	}
}
