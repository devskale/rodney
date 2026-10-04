package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"
	_ "image/jpeg"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/devices"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

//go:embed help.txt
var helpText string

// version is the devskale fork version. Overridable via -ldflags "-X main.version=..."
// for tagged releases, but the default keeps fork builds distinguishable from
// upstream (which reports plain "dev").
var version = "0.11.0" // devskale fork

// scopeMode determines whether to use a local or global state directory.
type scopeMode int

const (
	scopeAuto   scopeMode = iota // auto-detect: local if .rodney/state.json exists in cwd, else global
	scopeLocal                   // force local (./.rodney/)
	scopeGlobal                  // force global (~/.rodney/)
)

// activeStateDir is set once at startup based on --local/--global flags.
var activeStateDir string

// sessionName is set once at startup from --session <name> (see extractScopeArgs).
var sessionName string

// extractScopeArgs scans args for --local/--global/--session <name>, removes
// them, and returns the mode + session name. If both scope flags appear, the
// last one wins. --session routes state to ~/.rodney-sessions/<name>/ so
// parallel sessions each own their active_page (and their own Chrome when
// they start one) without stepping on each other.
func extractScopeArgs(args []string) (scopeMode, string, []string) {
	mode := scopeAuto
	session := ""
	filtered := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--local":
			mode = scopeLocal
		case "--global":
			mode = scopeGlobal
		case "--session":
			if i+1 < len(args) {
				session = args[i+1]
				i++ // skip the value
			} else {
				fmt.Fprintln(os.Stderr, "--session needs a name")
				os.Exit(1)
			}
		default:
			filtered = append(filtered, args[i])
		}
	}
	return mode, session, filtered
}

// resolveStateDir determines the state directory based on scope mode and working directory.
func resolveStateDir(mode scopeMode, workingDir string) string {
	switch mode {
	case scopeLocal:
		return filepath.Join(workingDir, ".rodney")
	case scopeGlobal:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".rodney")
	default: // scopeAuto
		localDir := filepath.Join(workingDir, ".rodney")
		if _, err := os.Stat(filepath.Join(localDir, "state.json")); err == nil {
			return localDir
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".rodney")
	}
}

// State persisted between CLI invocations
type State struct {
	DebugURL        string            `json:"debug_url"`
	ChromePID       int               `json:"chrome_pid"`
	ActivePage      int               `json:"active_page"` // index into pages list
	DataDir         string            `json:"data_dir"`
	ProxyPID        int               `json:"proxy_pid,omitempty"`  // PID of auth proxy helper
	ProxyPort       int               `json:"proxy_port,omitempty"` // local port of auth proxy
	VideoRecording  bool              `json:"video_recording,omitempty"`
	VideoDir        string            `json:"video_dir,omitempty"`
	ConsolePID      int               `json:"console_pid,omitempty"` // PID of background console collector
	ConsoleLog      string            `json:"console_log,omitempty"` // path to console.jsonl buffer
	RequestPID      int               `json:"request_pid,omitempty"` // PID of background request collector
	RequestLog      string            `json:"request_log,omitempty"` // path to requests.jsonl buffer
	Headers         map[string]string `json:"headers,omitempty"`     // extra HTTP headers applied to every request
	ViewportW       int               `json:"viewport_w,omitempty"`  // persisted viewport (0 = default)
	ViewportH       int               `json:"viewport_h,omitempty"`
	DeviceName      string            `json:"device_name,omitempty"` // persisted device emulation preset
	DeviceLandscape bool              `json:"device_landscape,omitempty"`
	Incognito       bool              `json:"incognito,omitempty"` // throwaway profile, removed on stop
	Onload          []string          `json:"onload,omitempty"`    // JS evaluated on every navigation
}

// stateDirOverride allows tests to redirect state to a temp dir
var stateDirOverride string

func stateDir() string {
	if stateDirOverride != "" {
		return stateDirOverride
	}
	if dir := os.Getenv("RODNEY_HOME"); dir != "" {
		return dir
	}
	if sessionName != "" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".rodney-sessions", sessionName)
	}
	if activeStateDir != "" {
		return activeStateDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".rodney")
}

func statePath() string {
	return filepath.Join(stateDir(), "state.json")
}

func loadState() (*State, error) {
	data, err := os.ReadFile(statePath())
	if err != nil {
		return nil, fmt.Errorf("no browser session (run 'rodney start' first)")
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("corrupt state file: %w", err)
	}
	return &s, nil
}

func saveState(s *State) error {
	if err := os.MkdirAll(stateDir(), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(), data, 0644)
}

func removeState() {
	os.Remove(statePath())
}

// connectBrowser connects to the running Chrome instance
func connectBrowser(s *State) (*rod.Browser, error) {
	browser := rod.New().ControlURL(s.DebugURL)
	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to browser (is it still running?): %w", err)
	}
	return browser, nil
}

// getActivePage returns the currently active page
func getActivePage(browser *rod.Browser, s *State) (*rod.Page, error) {
	pages, err := browser.Pages()
	if err != nil {
		return nil, fmt.Errorf("failed to list pages: %w", err)
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("no pages open")
	}
	idx := s.ActivePage
	if idx < 0 || idx >= len(pages) {
		idx = 0
	}
	return pages[idx], nil
}

func printUsage() {
	fmt.Print(helpText)
}

// commandUsage maps each command to its one-line usage string, used for
// per-command `rodney <cmd> --help` output.
var commandUsage = map[string]string{
	"start":          "rodney start [--show] [--insecure|-k] [--incognito] [--local]",
	"connect":        "rodney connect <host:port>",
	"stop":           "rodney stop",
	"status":         "rodney status",
	"open":           "rodney open <url> [--reuse]",
	"back":           "rodney back",
	"forward":        "rodney forward",
	"reload":         "rodney reload [--hard]",
	"clear-cache":    "rodney clear-cache",
	"url":            "rodney url",
	"title":          "rodney title",
	"html":           "rodney html [selector]",
	"text":           "rodney text <selector>",
	"attr":           "rodney attr <selector> <name>",
	"pdf":            "rodney pdf [file]",
	"js":             "rodney js <expression>",
	"click":          "rodney click <selector>",
	"input":          "rodney input <selector> <text>",
	"clear":          "rodney clear <selector>",
	"select":         "rodney select <selector> <value>",
	"submit":         "rodney submit <selector>",
	"hover":          "rodney hover <selector>",
	"file":           "rodney file <selector> <path|->",
	"download":       "rodney download <selector> [file|-]",
	"focus":          "rodney focus <selector>",
	"wait":           "rodney wait <selector> | rodney wait --url <substring>",
	"waitload":       "rodney waitload",
	"waitstable":     "rodney waitstable",
	"waitidle":       "rodney waitidle",
	"sleep":          "rodney sleep <seconds>",
	"screenshot":     "rodney screenshot [-w N] [-h N] [--full] [file]",
	"screenshot-el":  "rodney screenshot-el <selector> [file]",
	"start-video":    "rodney start-video",
	"stop-video":     "rodney stop-video [file]",
	"pages":          "rodney pages [--json]",
	"page":           "rodney page <index|t:targetID>",
	"newpage":        "rodney newpage [url]",
	"closepage":      "rodney closepage [index|t:targetID]",
	"press":          "rodney press <key> [key ...]",
	"type":           "rodney type <text>",
	"scroll":         "rodney scroll <x> <y> [--steps N]",
	"scroll-el":      "rodney scroll-el <selector>",
	"exists":         "rodney exists <selector>",
	"count":          "rodney count <selector>",
	"visible":        "rodney visible <selector>",
	"assert":         "rodney assert <js-expression> [expected] [-m msg]",
	"ua":             "rodney ua <user-agent>",
	"timezone":       "rodney timezone <timezone-id>",
	"locale":         "rodney locale <locale>",
	"geo":            "rodney geo --lat <lat> --lon <lon>",
	"media":          "rodney media [--type T] [--feature name=value ...]",
	"ax-tree":        "rodney ax-tree [--depth N] [--json]",
	"ax-find":        "rodney ax-find [--name N] [--role R] [--json]",
	"ax-node":        "rodney ax-node <selector> [--json]",
	"cookie-set":     "rodney cookie-set <name> <value> [--domain d] [--url u] [--path p] [--expires t] [--http-only] [--secure]",
	"cookie-get":     "rodney cookie-get [name] [--json]",
	"cookie-delete":  "rodney cookie-delete <name> [--domain d] [--url u] [--path p]",
	"cookie-clear":   "rodney cookie-clear [--domain <domain>]",
	"mock":           "rodney mock <pattern> <response> [--status N] [--type MIME] [--method M]",
	"block":          "rodney block <pattern> [--method M]",
	"console":        "rodney console [--level L] [--json] [--browser] [--follow] [--clear]",
	"console-start":  "rodney console-start",
	"console-stop":   "rodney console-stop",
	"dialog":         "rodney dialog [--dismiss] [--text MSG] [--json]",
	"requests":       "rodney requests [--json] [--follow] [--clear]",
	"requests-start": "rodney requests-start",
	"requests-stop":  "rodney requests-stop",
	"viewport":       "rodney viewport <width> <height> [--clear]",
	"device":         "rodney device <name> [--landscape] [--clear] [--list]",
	"waitnav":        "rodney waitnav",
	"headers":        "rodney headers [k=v ...] [--clear]",
	"resource":       "rodney resource <url> [file|-]",
	"history":        "rodney history",
	"drag":           "rodney drag <source-selector> <target-selector>",
	"onload":         "rodney onload <js> [--clear]",
	"waitpage":       "rodney waitpage [seconds]",
	"stopload":       "rodney stopload",
	"tap":            "rodney tap <selector>",
	"xpath-of":       "rodney xpath-of <selector>",
	"filechooser":    "rodney filechooser <path>",
	"monitor":        "rodney monitor [host:port]",
	"doctor":         "rodney doctor",
}

// cmdHelpEntry is the structured help for one command (Tier 1/2 of the help
// system: `rodney help <cmd>`, `rodney <cmd> --help`, `rodney help --json`).
type cmdHelpEntry struct {
	Group    string   `json:"group"`
	Usage    string   `json:"usage"`
	Desc     string   `json:"description"`
	Flags    []string `json:"flags,omitempty"`
	Examples []string `json:"examples,omitempty"`
}

// commandHelp holds the per-command help registry. Usage lines mirror
// commandUsage; Desc/Flags/Examples power progressive discovery for agents.
var commandHelp = map[string]cmdHelpEntry{
	// --- Browser lifecycle ---
	"start": {Group: "Browser lifecycle", Usage: "rodney start [--show] [--insecure|-k] [--incognito] [--local]",
		Desc:     "Launch Chrome (headless by default) and save the connection state. Other rodney commands reuse this browser.",
		Flags:    []string{"--show      launch visible Chrome instead of headless", "--insecure, -k   ignore certificate errors", "--incognito  throwaway profile, deleted on stop", "--local    directory-scoped session (./.rodney/)"},
		Examples: []string{"rodney start", "rodney start --show --insecure"}},
	"connect": {Group: "Browser lifecycle", Usage: "rodney connect <host:port>",
		Desc:     "Attach to an already-running Chrome's remote debug port instead of launching one.",
		Examples: []string{"rodney connect 127.0.0.1:9222"}},
	"stop": {Group: "Browser lifecycle", Usage: "rodney stop",
		Desc: "Shut down Chrome, the auth proxy, the console collector; clear session state.", Examples: []string{"rodney stop"}},
	"status": {Group: "Browser lifecycle", Usage: "rodney status",
		Desc: "Show browser status: running, debug URL, active page, current URL.", Examples: []string{"rodney status"}},

	// --- Navigation ---
	"open": {Group: "Navigation", Usage: "rodney open <url> [--reuse]",
		Desc:     "Navigate the active page to a URL. http:// is added if no scheme is given.",
		Flags:    []string{"--reuse    switch to an existing page already at that URL instead of navigating the active page away"},
		Examples: []string{"rodney open https://example.com", "rodney open https://example.com --reuse"}},
	"back":    {Group: "Navigation", Usage: "rodney back", Desc: "Go back one step in history.", Examples: []string{"rodney back"}},
	"forward": {Group: "Navigation", Usage: "rodney forward", Desc: "Go forward one step in history.", Examples: []string{"rodney forward"}},
	"reload": {Group: "Navigation", Usage: "rodney reload [--hard]",
		Desc: "Reload the page.", Flags: []string{"--hard   bypass the cache"}, Examples: []string{"rodney reload --hard"}},
	"clear-cache": {Group: "Navigation", Usage: "rodney clear-cache", Desc: "Clear the browser cache.", Examples: []string{"rodney clear-cache"}},

	// --- Page info ---
	"url":   {Group: "Page info", Usage: "rodney url", Desc: "Print the current URL.", Examples: []string{"rodney url"}},
	"title": {Group: "Page info", Usage: "rodney title", Desc: "Print the page title.", Examples: []string{"rodney title"}},
	"html": {Group: "Page info", Usage: "rodney html [selector]",
		Desc:     "Print HTML of the page or of one element (pretty-printed).",
		Examples: []string{"rodney html", "rodney html \"#main\""}},
	"text": {Group: "Page info", Usage: "rodney text <selector>",
		Desc: "Print the text content of an element.", Examples: []string{"rodney text \"h1\""}},
	"attr": {Group: "Page info", Usage: "rodney attr <selector> <name>",
		Desc: "Print an attribute value of an element.", Examples: []string{"rodney attr \"a.link\" href"}},
	"pdf": {Group: "Page info", Usage: "rodney pdf [file]",
		Desc: "Save the page as PDF (default: <title>.pdf).", Examples: []string{"rodney pdf out.pdf"}},

	// --- Interaction ---
	"js": {Group: "Interaction", Usage: "rodney js <expression>",
		Desc:     "Evaluate a JavaScript expression on the active page. Bare expressions are auto-wrapped; statements like console.log work via the wrapper.",
		Examples: []string{"rodney js \"document.title\"", "rodney js \"1 + 1\""}},
	"click": {Group: "Interaction", Usage: "rodney click <selector>",
		Desc: "Click an element (waits for it to appear).", Examples: []string{"rodney click \"button#submit\""}},
	"input": {Group: "Interaction", Usage: "rodney input <selector> <text>",
		Desc:     "Type text into an input field by setting .value (fast, but fires no key events — use 'type' or 'press' if the page reacts to keyboard input).",
		Examples: []string{"rodney input \"#search\" \"query\""}},
	"type": {Group: "Interaction", Usage: "rodney type <text>",
		Desc:     "Type text into the focused element as real keyboard input — fires key and input events (SPA validation, autocomplete react to it).",
		Examples: []string{"rodney focus \"#search\" && rodney type \"query\""}},
	"press": {Group: "Interaction", Usage: "rodney press <key> [key ...]",
		Desc:     "Press keys as real keyboard events. Combos use +; multiple args press in sequence.",
		Flags:    []string{"keys: enter, tab, escape, backspace, delete, space, up, down, left, right, home, end, pageup, pagedown, shift, ctrl, alt, meta, or a single character"},
		Examples: []string{"rodney press enter", "rodney press ctrl+a", "rodney press shift tab"}},
	"clear": {Group: "Interaction", Usage: "rodney clear <selector>",
		Desc: "Clear an input field.", Examples: []string{"rodney clear \"#search\""}},
	"file": {Group: "Interaction", Usage: "rodney file <selector> <path|->",
		Desc:     "Set a file on a file input element. '-' reads the content from stdin.",
		Examples: []string{"rodney file \"#upload\" photo.png", "cat data.csv | rodney file \"#upload\" -"}},
	"download": {Group: "Interaction", Usage: "rodney download <selector> [file|-]",
		Desc:     "Download the href/src target of an element. '-' streams to stdout.",
		Examples: []string{"rodney download \"a.pdf\"", "rodney download \"img.logo\" -"}},
	"select": {Group: "Interaction", Usage: "rodney select <selector> <value>",
		Desc: "Select a dropdown option by value.", Examples: []string{"rodney select \"#topic\" \"support\""}},
	"submit": {Group: "Interaction", Usage: "rodney submit <selector>",
		Desc: "Submit a form.", Examples: []string{"rodney submit \"form#login\""}},
	"hover": {Group: "Interaction", Usage: "rodney hover <selector>",
		Desc: "Hover over an element (triggers mouseenter/mouseover).", Examples: []string{"rodney hover \".menu-item\""}},
	"focus": {Group: "Interaction", Usage: "rodney focus <selector>",
		Desc: "Focus an element (use before 'type' or 'press').", Examples: []string{"rodney focus \"#email\""}},
	"scroll": {Group: "Interaction", Usage: "rodney scroll <x> <y> [--steps N]",
		Desc:     "Scroll the page by x/y pixels (relative; negative y scrolls up).",
		Flags:    []string{"--steps N   scroll in N increments (smooth scroll, lazy-loading)"},
		Examples: []string{"rodney scroll 0 600", "rodney scroll 0 1000 --steps 10"}},
	"scroll-el": {Group: "Interaction", Usage: "rodney scroll-el <selector>",
		Desc: "Scroll an element into view.", Examples: []string{"rodney scroll-el \"#footer\""}},

	// --- Waiting ---
	"wait": {Group: "Waiting", Usage: "rodney wait <selector> | rodney wait --url <substring>",
		Desc: "Wait until an element appears (default timeout 30s, ROD_TIMEOUT to change).", Examples: []string{"rodney wait \".results\""}},
	"waitload":   {Group: "Waiting", Usage: "rodney waitload", Desc: "Wait for the page load event.", Examples: []string{"rodney waitload"}},
	"waitstable": {Group: "Waiting", Usage: "rodney waitstable", Desc: "Wait until the DOM stops changing.", Examples: []string{"rodney waitstable"}},
	"waitidle":   {Group: "Waiting", Usage: "rodney waitidle", Desc: "Wait until the network is idle.", Examples: []string{"rodney waitidle"}},
	"sleep":      {Group: "Waiting", Usage: "rodney sleep <seconds>", Desc: "Sleep for N seconds.", Examples: []string{"rodney sleep 2"}},

	// --- Screenshots ---
	"screenshot": {Group: "Screenshots", Usage: "rodney screenshot [-w N] [-h N] [--full] [file]",
		Desc:     "Take a PNG screenshot. Default captures the FULL page; -h limits to the viewport height. '-' for stdout.",
		Flags:    []string{"-w N       viewport width (default 1280)", "-h N       viewport height (limits capture to the viewport)", "--full     force full-page capture even with -h"},
		Examples: []string{"rodney screenshot", "rodney screenshot --full page.png"}},
	// --- Dialogs ---
	"dialog": {Group: "Dialogs", Usage: "rodney dialog [--dismiss] [--text MSG] [--json]",
		Desc:     "Handle JavaScript dialogs (alert/confirm/prompt/beforeunload). Runs as a persistent foreground process: handles every dialog that opens until Ctrl+C. Default accepts dialogs; use --dismiss to reject them.",
		Flags:    []string{"--dismiss   dismiss (cancel) instead of accepting", "--text MSG   response text for prompt dialogs", "--json      JSON lines output"},
		Examples: []string{"rodney open page.html && rodney dialog", "rodney dialog --dismiss"}},
	"screenshot-el": {Group: "Screenshots", Usage: "rodney screenshot-el <selector> [file]",
		Desc: "Screenshot a single element.", Examples: []string{"rodney screenshot-el \"#chart\""}},

	// --- Video recording ---
	"start-video": {Group: "Video recording", Usage: "rodney start-video",
		Desc: "Start recording the page as video frames (saved on stop-video).", Examples: []string{"rodney start-video"}},
	"stop-video": {Group: "Video recording", Usage: "rodney stop-video [file]",
		Desc: "Stop recording and save. .gif by default; .mp4 requires ffmpeg in PATH.", Examples: []string{"rodney stop-video demo.gif"}},

	// --- Tabs ---
	"pages": {Group: "Tabs", Usage: "rodney pages [--json]",
		Desc:     "List all pages/tabs. * marks the active one.",
		Flags:    []string{"--json   machine-readable: index, target, title, url, active"},
		Examples: []string{"rodney pages", "rodney pages --json"}},
	"page": {Group: "Tabs", Usage: "rodney page <index|t:targetID>",
		Desc:     "Switch the active page. t:<id> pins by stable target ID — drift-proof when parallel sessions shift indices.",
		Examples: []string{"rodney page 1", "rodney page t:ABC123"}},
	"newpage": {Group: "Tabs", Usage: "rodney newpage [url]", Desc: "Open a new page/tab and make it active.", Examples: []string{"rodney newpage https://example.com"}},
	"closepage": {Group: "Tabs", Usage: "rodney closepage [index|t:targetID]",
		Desc:     "Close a page (default: the active one). t:<id> is drift-proof; the active index is adjusted automatically.",
		Examples: []string{"rodney closepage", "rodney closepage t:ABC123"}},

	// --- Element checks ---
	"exists": {Group: "Element checks", Usage: "rodney exists <selector>",
		Desc: "Check if an element exists. Exit 0 if yes, exit 1 if not.", Examples: []string{"rodney exists \".error\""}},
	"count": {Group: "Element checks", Usage: "rodney count <selector>", Desc: "Count matching elements.", Examples: []string{"rodney count \"li.item\""}},
	"visible": {Group: "Element checks", Usage: "rodney visible <selector>",
		Desc: "Check if an element is visible. Exit 0/1.", Examples: []string{"rodney visible \"#modal\""}},
	"assert": {Group: "Element checks", Usage: "rodney assert <js-expression> [expected] [-m msg]",
		Desc:     "Assert a JS expression is truthy (or equals 'expected'). Exit 1 on failure.",
		Flags:    []string{"-m, --message msg   custom failure message"},
		Examples: []string{"rodney assert \"document.title\" \"Dashboard\"", "rodney assert \"document.querySelectorAll('.row').length\" 5"}},

	// --- Cookies ---
	"cookie-set": {Group: "Cookies", Usage: "rodney cookie-set <name> <value> [--domain d] [--url u] [--path p] [--expires t] [--http-only] [--secure]",
		Desc:     "Set a cookie. Defaults to the current page's URL/domain.",
		Examples: []string{"rodney cookie-set session abc123"}},
	"cookie-get": {Group: "Cookies", Usage: "rodney cookie-get [name] [--json]",
		Desc:     "Get cookies: one value by name, or all cookies (--json for structured output).",
		Examples: []string{"rodney cookie-get", "rodney cookie-get session --json"}},
	"cookie-delete": {Group: "Cookies", Usage: "rodney cookie-delete <name> [--domain d] [--url u] [--path p]",
		Desc: "Delete cookies by name (scoped by domain/url/path if given).", Examples: []string{"rodney cookie-delete session"}},
	"cookie-clear": {Group: "Cookies", Usage: "rodney cookie-clear [--domain <domain>]",
		Desc: "Clear all cookies, or only those of one domain.", Examples: []string{"rodney cookie-clear --domain example.com"}},

	// --- Emulation ---
	"ua": {Group: "Emulation", Usage: "rodney ua <user-agent>",
		Desc: "Override the browser user agent string.", Examples: []string{"rodney ua \"Mozilla/5.0 (iPhone)\""}},
	"timezone": {Group: "Emulation", Usage: "rodney timezone <timezone-id>",
		Desc: "Override the timezone (what Date/timezone APIs report).", Examples: []string{"rodney timezone Asia/Tokyo"}},
	"locale": {Group: "Emulation", Usage: "rodney locale <locale>",
		Desc: "Override the locale (what Intl APIs report).", Examples: []string{"rodney locale de-DE"}},
	"geo": {Group: "Emulation", Usage: "rodney geo --lat <lat> --lon <lon>",
		Desc: "Spoof geolocation coordinates.", Examples: []string{"rodney geo --lat 52.52 --lon 13.40"}},
	"media": {Group: "Emulation", Usage: "rodney media [--type T] [--feature name=value ...]",
		Desc:     "Emulate media type or features (e.g. prefers-color-scheme).",
		Examples: []string{"rodney media --type print", "rodney media --feature prefers-color-scheme=dark"}},

	// --- Network interception ---
	"mock": {Group: "Network interception", Usage: "rodney mock <pattern> <response> [--status N] [--type MIME] [--method M]",
		Desc:     "Serve a canned response for matching requests. Runs as a persistent foreground process — Ctrl+C to stop.",
		Flags:    []string{"--status N    HTTP status (default 200)", "--type MIME   content type (default text/plain)", "--method M    restrict to HTTP method", "response '-' reads body from stdin"},
		Examples: []string{"rodney mock '*api/users*' '{\"id\":1}' --type application/json"}},
	"block": {Group: "Network interception", Usage: "rodney block <pattern> [--method M]",
		Desc:     "Fail matching requests client-side (offline behaviour). Persistent foreground process.",
		Examples: []string{"rodney block '*.ads.*'"}},

	// --- Console ---
	"console": {Group: "Console", Usage: "rodney console [--level L] [--json] [--browser] [--follow] [--clear]",
		Desc:     "Read console output. Without a background collector: live stream (Ctrl+C). With collector (console-start): print buffered messages.",
		Flags:    []string{"--level L    filter: log, info, warn, error, debug", "--json       JSON lines output", "--browser    also browser-level log entries (network errors, security)", "--follow     print buffered, then tail live", "--clear      print and empty the buffer"},
		Examples: []string{"rodney console --level error", "rodney console --json | jq 'select(.type==\"error\")'"}},
	"console-start": {Group: "Console", Usage: "rodney console-start",
		Desc: "Start the background console collector — captures console/browser logs between commands into console.jsonl.", Examples: []string{"rodney console-start"}},
	"console-stop": {Group: "Console", Usage: "rodney console-stop",
		Desc: "Stop the collector and remove the buffer.", Examples: []string{"rodney console-stop"}},

	// --- Network requests ---
	"requests": {Group: "Network requests", Usage: "rodney requests [--json] [--follow] [--clear]",
		Desc:     "Read captured network requests. Without a background collector: live stream (Ctrl+C). With collector (requests-start): print buffered requests.",
		Flags:    []string{"--json    JSON lines output", "--follow  print buffered, then tail live", "--clear   print and empty the buffer"},
		Examples: []string{"rodney requests", "rodney requests --json | jq 'select(.status>=400)'"}},
	"requests-start": {Group: "Network requests", Usage: "rodney requests-start",
		Desc: "Start the background request collector — captures all requests/responses between commands into requests.jsonl.", Examples: []string{"rodney requests-start"}},
	"requests-stop": {Group: "Network requests", Usage: "rodney requests-stop",
		Desc: "Stop the request collector and remove the buffer.", Examples: []string{"rodney requests-stop"}},

	// --- Accessibility ---
	"ax-tree": {Group: "Accessibility", Usage: "rodney ax-tree [--depth N] [--json]",
		Desc:     "Dump the accessibility tree (roles, names, states).",
		Flags:    []string{"--depth N   limit tree depth", "--json      structured output"},
		Examples: []string{"rodney ax-tree --depth 3"}},
	"ax-find": {Group: "Accessibility", Usage: "rodney ax-find [--name N] [--role R] [--json]",
		Desc:     "Find accessible nodes by name/role. Exit 1 if no match.",
		Examples: []string{"rodney ax-find --role button --name Checkout"}},
	"ax-node": {Group: "Accessibility", Usage: "rodney ax-node <selector> [--json]",
		Desc: "Show accessibility info for one element.", Examples: []string{"rodney ax-node \"#submit\""}},

	// --- Viewport & device emulation ---
	"viewport": {Group: "Viewport & device emulation", Usage: "rodney viewport <width> <height> [--clear]",
		Desc:     "Set the page viewport size (affects layout, screenshots, innerWidth). Persists on the page until cleared or the browser stops.",
		Flags:    []string{"--clear   reset to default viewport"},
		Examples: []string{"rodney viewport 1280 800", "rodney screenshot"}},
	"device": {Group: "Viewport & device emulation", Usage: "rodney device <name> [--landscape] [--clear] [--list]",
		Desc:     "Emulate a device: viewport, device pixel ratio, touch, and user agent in one step.",
		Flags:    []string{"--landscape   use the landscape orientation", "--clear   stop emulating", "--list    list available devices"},
		Examples: []string{"rodney device iphone-x", "rodney device pixel-2 --landscape", "rodney device --list"}},
	"headers": {Group: "Viewport & device emulation", Usage: "rodney headers [k=v ...] [--clear]",
		Desc:     "Set extra HTTP headers sent with every request on this session (e.g. auth tokens, API versioning). No args: list current. Persists in the session state.",
		Flags:    []string{"--clear   remove all extra headers"},
		Examples: []string{"rodney headers Authorization=Bearer tok", "rodney headers X-Api-Version=2", "rodney headers"}},
	"waitnav": {Group: "Waiting", Usage: "rodney waitnav",
		Desc: "Wait until the page navigates to a different URL (redirects, form submits, SPA route changes).", Examples: []string{"rodney waitnav"}},

	// --- Page info extras ---
	"resource": {Group: "Page info", Usage: "rodney resource <url> [file|-]",
		Desc:     "Print the cached body of an already-loaded resource (script, XHR response, image) — no new request. Substring URL match.",
		Examples: []string{"rodney resource /api/users", "rodney resource app.js script.js"}},
	"history": {Group: "Page info", Usage: "rodney history",
		Desc: "Print the navigation history of the active page (* marks current).", Examples: []string{"rodney history"}},
	"stopload": {Group: "Page info", Usage: "rodney stopload",
		Desc: "Stop the page's pending navigation and resource fetches — proceed with a half-loaded page (scraping speed).", Examples: []string{"rodney stopload"}},

	// --- Advanced interaction ---
	"drag": {Group: "Advanced interaction", Usage: "rodney drag <source-selector> <target-selector>",
		Desc:     "Drag an element onto another via real mouse events (down, move, up) — sliders, sortables, kanban boards.",
		Examples: []string{"rodney drag \".card\" \"#done-column\""}},
	"tap": {Group: "Advanced interaction", Usage: "rodney tap <selector>",
		Desc: "Tap an element with touch semantics (pairs with `rodney device` emulation).", Examples: []string{"rodney device iphone-x && rodney tap \"#menu\""}},
	"xpath-of": {Group: "Advanced interaction", Usage: "rodney xpath-of <selector>",
		Desc: "Print the computed XPath of an element — helps building XPath queries.", Examples: []string{"rodney xpath-of \"h1\""}},
	"onload": {Group: "Advanced interaction", Usage: "rodney onload <js> [--clear]",
		Desc:     "Register JS that runs on EVERY navigation of the session (persisted) — hide cookie banners, inject test hooks, stub globals. No args: list.",
		Flags:    []string{"--clear   remove all onload scripts"},
		Examples: []string{`rodney onload "document.querySelector('.banner')?.remove()"`, "rodney onload", "rodney onload --clear"}},
	"filechooser": {Group: "Advanced interaction", Usage: "rodney filechooser <path>",
		Desc:     "Intercept file choosers as a persistent foreground process: every chooser the page opens gets the given file, until Ctrl+C.",
		Examples: []string{"rodney filechooser upload.png"}},
	"waitpage": {Group: "Waiting", Usage: "rodney waitpage [seconds]",
		Desc: "Wait until a NEW page/tab opens (window.open, target=_blank, OAuth popups) and switch the active page to it.", Examples: []string{"rodney waitpage 30"}},
	"monitor": {Group: "Debugging", Usage: "rodney monitor [host:port]",
		Desc: "Serve rod's live monitor web UI (pages, eval console, request log). Foreground process.", Examples: []string{"rodney monitor"}},
	"doctor": {Group: "Debugging", Usage: "rodney doctor",
		Desc: "Self-diagnostics: version, Chrome detection, ffmpeg, session state, browser connectivity. Exit 2 if any check fails.", Examples: []string{"rodney doctor"}},
}

// printCommandHelp renders the Tier-1 help block for one command.
func printCommandHelp(name string) {
	e, ok := commandHelp[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "no detailed help for %s\n", name)
		os.Exit(2)
	}
	fmt.Printf("%s — %s\n\n", name, e.Desc)
	fmt.Printf("Usage: %s\n", e.Usage)
	if len(e.Flags) > 0 {
		fmt.Printf("\nFlags:\n")
		for _, f := range e.Flags {
			fmt.Printf("  %s\n", f)
		}
	}
	if len(e.Examples) > 0 {
		fmt.Printf("\nExamples:\n")
		for _, ex := range e.Examples {
			fmt.Printf("  %s\n", ex)
		}
	}
	fmt.Printf("\nGroup: %s\n", e.Group)
}

// printHelpRegistry prints the whole command registry as JSON (Tier 2).
func printHelpRegistryJSON() {
	b, err := json.MarshalIndent(commandHelp, "", "  ")
	if err != nil {
		fatal("failed to marshal registry: %v", err)
	}
	fmt.Println(string(b))
}

// levenshtein computes the edit distance between two strings (lowercased).
func levenshtein(a, b string) int {
	a, b = strings.ToLower(a), strings.ToLower(b)
	ra, rb := []rune(a), []rune(b)
	d := make([][]int, len(ra)+1)
	for i := range d {
		d[i] = make([]int, len(rb)+1)
		d[i][0] = i
	}
	for j := 0; j <= len(rb); j++ {
		d[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			min := d[i-1][j] + 1             // deletion
			if v := d[i][j-1] + 1; v < min { // insertion
				min = v
			}
			if v := d[i-1][j-1] + cost; v < min { // substitution
				min = v
			}
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] { // transposition
				if v := d[i-2][j-2] + 1; v < min {
					min = v
				}
			}
			d[i][j] = min
		}
	}
	return d[len(ra)][len(rb)]
}

// suggestCommand returns the closest known command name, or "" if nothing
// is close. Prefix match always wins; otherwise Damerau-Levenshtein distance
// must be 1 (substitution, insertion, deletion, or adjacent transposition).
func suggestCommand(name string) string {
	name = strings.ToLower(name)
	best, bestDist := "", 99
	for known := range commandUsage {
		if strings.HasPrefix(known, name) {
			return known // prefix match is a strong signal
		}
		if d := levenshtein(name, known); d < bestDist {
			best, bestDist = known, d
		}
	}
	if bestDist <= 1 {
		return best
	}
	return ""
}

// containsHelpFlag reports whether args contains a --help request.
func containsHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "--help" {
			return true
		}
	}
	return false
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(2)
}

// findUnknownFlag returns the first arg not registered in fs, preserving original form (e.g. --bogus).
func findUnknownFlag(args []string, fs *flag.FlagSet) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if fs.Lookup(name) == nil {
			return a
		}
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func main() {
	defer func() {
		if videoCleanup != nil {
			videoCleanup()
		}
	}()

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	// Extract --local/--global from all args before dispatching
	mode, session, cleanedArgs := extractScopeArgs(os.Args[1:])
	if len(cleanedArgs) == 0 {
		printUsage()
		os.Exit(1)
	}

	wd, _ := os.Getwd()
	sessionName = session
	activeStateDir = resolveStateDir(mode, wd)

	cmd := cleanedArgs[0]
	args := cleanedArgs[1:]

	if cmd == "--version" {
		fmt.Println(version)
		os.Exit(0)
	}

	// Per-command help: if the command is known and --help is present among
	// its args, print the structured Tier-1 help instead of dispatching. This
	// gives a uniform `rodney <cmd> --help` for every command, including
	// flag-based ones that would otherwise choke on the flag parser. `-h` is
	// intentionally NOT treated as help here because some commands (e.g.
	// screenshot) use it as a flag alias.
	if _, known := commandHelp[cmd]; known {
		if containsHelpFlag(args) {
			printCommandHelp(cmd)
			os.Exit(0)
		}
	}

	switch cmd {
	case "_proxy":
		cmdInternalProxy(args) // hidden: runs the auth proxy helper
	case "_console":
		cmdInternalConsole(args) // hidden: runs the console collector
	case "_requests":
		cmdInternalRequests(args) // hidden: runs the request collector
	case "start":
		cmdStart(args)
	case "connect":
		cmdConnect(args)
	case "stop":
		cmdStop(args)
	case "status":
		cmdStatus(args)
	case "open":
		cmdOpen(args)
	case "back":
		cmdBack(args)
	case "forward":
		cmdForward(args)
	case "reload":
		cmdReload(args)
	case "clear-cache":
		cmdClearCache(args)
	case "url":
		cmdURL(args)
	case "title":
		cmdTitle(args)
	case "html":
		cmdHTML(args)
	case "text":
		cmdText(args)
	case "attr":
		cmdAttr(args)
	case "pdf":
		cmdPDF(args)
	case "js":
		cmdJS(args)
	case "click":
		cmdClick(args)
	case "input":
		cmdInput(args)
	case "clear":
		cmdClear(args)
	case "select":
		cmdSelect(args)
	case "submit":
		cmdSubmit(args)
	case "hover":
		cmdHover(args)
	case "file":
		cmdFile(args)
	case "download":
		cmdDownload(args)
	case "mock":
		cmdMock(args)
	case "block":
		cmdBlock(args)
	case "focus":
		cmdFocus(args)
	case "wait":
		cmdWait(args)
	case "waitload":
		cmdWaitLoad(args)
	case "waitstable":
		cmdWaitStable(args)
	case "waitidle":
		cmdWaitIdle(args)
	case "sleep":
		cmdSleep(args)
	case "screenshot":
		cmdScreenshot(args)
	case "screenshot-el":
		cmdScreenshotEl(args)
	case "start-video":
		cmdStartVideo(args)
	case "stop-video":
		cmdStopVideo(args)
	case "pages":
		cmdPages(args)
	case "page":
		cmdPage(args)
	case "newpage":
		cmdNewPage(args)
	case "closepage":
		cmdClosePage(args)
	case "press":
		cmdPress(args)
	case "type":
		cmdType(args)
	case "scroll":
		cmdScroll(args)
	case "scroll-el":
		cmdScrollEl(args)
	case "exists":
		cmdExists(args)
	case "count":
		cmdCount(args)
	case "visible":
		cmdVisible(args)
	case "assert":
		cmdAssert(args)
	case "ua":
		cmdUA(args)
	case "timezone":
		cmdTimezone(args)
	case "locale":
		cmdLocale(args)
	case "geo":
		cmdGeo(args)
	case "media":
		cmdMedia(args)
	case "ax-tree":
		cmdAXTree(args)
	case "ax-find":
		cmdAXFind(args)
	case "ax-node":
		cmdAXNode(args)
	case "cookie-set":
		cmdCookieSet(args)
	case "cookie-get":
		cmdCookieGet(args)
	case "cookie-delete":
		cmdCookieDelete(args)
	case "cookie-clear":
		cmdCookieClear(args)
	case "console":
		cmdConsole(args)
	case "console-start":
		cmdConsoleStart(args)
	case "console-stop":
		cmdConsoleStop(args)
	case "dialog":
		cmdDialog(args)
	case "requests":
		cmdRequests(args)
	case "requests-start":
		cmdRequestsStart(args)
	case "requests-stop":
		cmdRequestsStop(args)
	case "viewport":
		cmdViewport(args)
	case "device":
		cmdDevice(args)
	case "waitnav":
		cmdWaitNav(args)
	case "headers":
		cmdHeaders(args)
	case "resource":
		cmdResource(args)
	case "history":
		cmdHistory(args)
	case "drag":
		cmdDrag(args)
	case "onload":
		cmdOnload(args)
	case "waitpage":
		cmdWaitPage(args)
	case "stopload":
		cmdStopLoad(args)
	case "tap":
		cmdTap(args)
	case "xpath-of":
		cmdXPathOf(args)
	case "filechooser":
		cmdFileChooser(args)
	case "monitor":
		cmdMonitor(args)
	case "doctor":
		cmdDoctor(args)
	case "help", "-h", "--help":
		// Tiered help: `help` = overview, `help <cmd>` = structured details,
		// `help --json` = full machine-readable registry.
		if len(args) > 0 {
			if args[0] == "--json" {
				printHelpRegistryJSON()
				os.Exit(0)
			}
			if _, known := commandHelp[args[0]]; known {
				printCommandHelp(args[0])
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
			if s := suggestCommand(args[0]); s != "" {
				fmt.Fprintf(os.Stderr, "did you mean: %s?\n", s)
			}
			fmt.Fprintf(os.Stderr, "run 'rodney help --json' for the full registry\n")
			os.Exit(2)
		}
		printUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		if s := suggestCommand(cmd); s != "" {
			fmt.Fprintf(os.Stderr, "did you mean: %s?\n", s)
		}
		fmt.Fprintf(os.Stderr, "run 'rodney --help' for all commands, 'rodney help --json' for the machine-readable registry\n")
		os.Exit(2)
	}
}

// Default timeout for element queries (seconds)
var defaultTimeout = 30 * time.Second

func init() {
	if t := os.Getenv("ROD_TIMEOUT"); t != "" {
		if secs, err := strconv.ParseFloat(t, 64); err == nil {
			defaultTimeout = time.Duration(secs * float64(time.Second))
		}
	}
}

// videoCleanup is called at process exit to flush any in-progress video capture.
var videoCleanup func()

// maybeStartVideoCapture checks state and starts screencast if recording is active.
// Returns a cleanup function (always safe to call, even if recording is off).
func maybeStartVideoCapture(page *rod.Page) func() {
	s, err := loadState()
	if err != nil || !s.VideoRecording || s.VideoDir == "" {
		return func() {}
	}
	stop := startVideoCapture(page, s.VideoDir)
	return func() { stop() }
}

// withPage loads state, connects, and returns the active page.
// Caller should NOT close the browser (we just disconnect).
func withPage() (*State, *rod.Browser, *rod.Page) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	page, err := getActivePage(browser, s)
	if err != nil {
		fatal("%v", err)
	}
	// Apply default timeout so element queries don't hang forever
	page = page.Timeout(defaultTimeout)
	applySessionOverrides(s, page)
	// Start video capture if recording is active
	videoCleanup = maybeStartVideoCapture(page)
	return s, browser, page
}

// applySessionOverrides re-applies persisted session state (device emulation,
// viewport, onload scripts, extra headers) to a page. rod resets to its
// default device (1280x800 + UA) on every new session attach, and
// EvalOnNewDocument/SetExtraHeaders registrations die with the process —
// so every connection must re-apply them.
func applySessionOverrides(s *State, page *rod.Page) {
	if s.DeviceName != "" {
		if dev, ok := devicePresets[s.DeviceName]; ok {
			if s.DeviceLandscape {
				dev = dev.Landscape()
			}
			_ = page.Emulate(dev)
		}
	} else if s.ViewportW > 0 && s.ViewportH > 0 {
		_ = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
			Width: s.ViewportW, Height: s.ViewportH, DeviceScaleFactor: 1,
		})
	}
	for _, js := range s.Onload {
		_, _ = page.EvalOnNewDocument(js)
	}
	if len(s.Headers) > 0 {
		dict := make([]string, 0, len(s.Headers)*2)
		for k, v := range s.Headers {
			dict = append(dict, k, v)
		}
		_, _ = page.SetExtraHeaders(dict)
	}
}

// --- Commands ---

// parseStartArgs parses the flags for the "start" command.
// Returns ignoreCertErrors, headless, incognito, and an error for unknown flags.
func parseStartArgs(args []string) (ignoreCertErrors bool, headless bool, incognito bool, err error) {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&ignoreCertErrors, "insecure", false, "")
	fs.BoolVar(&ignoreCertErrors, "k", false, "")
	show := fs.Bool("show", false, "")
	incognitoFlag := fs.Bool("incognito", false, "")

	if parseErr := fs.Parse(args); parseErr != nil {
		return false, true, false, fmt.Errorf("unknown flag: %s\nusage: rodney start [--show] [--insecure] [--incognito]", findUnknownFlag(args, fs))
	}
	if fs.NArg() > 0 {
		return false, true, false, fmt.Errorf("unknown flag: %s\nusage: rodney start [--show] [--insecure] [--incognito]", fs.Arg(0))
	}
	headless = !*show
	return ignoreCertErrors, headless, *incognitoFlag, nil
}

func cmdStart(args []string) {
	ignoreCertErrors, headless, incognito, err := parseStartArgs(args)
	if err != nil {
		fatal("%s", err)
	}

	// Check if already running
	if s, err := loadState(); err == nil {
		// Try connecting
		if b, err := connectBrowser(s); err == nil {
			b.MustClose()
			// It was actually running, warn
			removeState()
		}
	}

	dataDir := filepath.Join(stateDir(), "chrome-data")
	if incognito {
		// Throwaway profile: temp dir, removed on `rodney stop`.
		tmp, err := os.MkdirTemp("", "rodney-incognito-*")
		if err != nil {
			fatal("failed to create incognito profile dir: %v", err)
		}
		dataDir = tmp
	}
	os.MkdirAll(dataDir, 0755)

	l := launcher.New().
		Set("no-sandbox").
		Set("disable-gpu").
		Set("single-process"). // Required for screenshots in gVisor/container environments
		Leakless(false).       // Keep Chrome alive after CLI exits
		UserDataDir(dataDir).
		Headless(headless)

	// When in non-headless mode, make sure that we show the startup window immediately
	// (instead of showing a window only after calling "rodney open")
	if !headless {
		l = l.Delete("no-startup-window")
	}

	if bin := os.Getenv("ROD_CHROME_BIN"); bin != "" {
		l = l.Bin(bin)
	}

	// Detect authenticated proxy and launch helper if needed
	var proxyPID, proxyPort int
	if server, user, pass, needed := detectProxy(); needed {
		authHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))

		// Find a free port for the local proxy
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fatal("failed to find free port for proxy: %v", err)
		}
		proxyPort = ln.Addr().(*net.TCPAddr).Port
		ln.Close()

		// Launch ourselves as the proxy helper in the background
		exe, _ := os.Executable()
		cmd := exec.Command(exe, "_proxy",
			strconv.Itoa(proxyPort), server, authHeader)
		setSysProcAttr(cmd)
		if err := cmd.Start(); err != nil {
			fatal("failed to start proxy helper: %v", err)
		}
		proxyPID = cmd.Process.Pid
		// Detach so it survives after we exit
		cmd.Process.Release()

		// Wait for the proxy to be ready
		time.Sleep(500 * time.Millisecond)

		l.Set("proxy-server", fmt.Sprintf("http://127.0.0.1:%d", proxyPort))
		ignoreCertErrors = true // Proxy requires ignoring cert errors
		fmt.Printf("Auth proxy started (PID %d, port %d) -> %s\n", proxyPID, proxyPort, server)
	}

	if ignoreCertErrors {
		l.Set("ignore-certificate-errors")
	}

	debugURL := l.MustLaunch()

	// Get Chrome PID from the launcher
	pid := l.PID()

	state := &State{
		DebugURL:   debugURL,
		ChromePID:  pid,
		ActivePage: 0,
		DataDir:    dataDir,
		ProxyPID:   proxyPID,
		ProxyPort:  proxyPort,
		Incognito:  incognito,
	}

	if err := saveState(state); err != nil {
		fatal("failed to save state: %v", err)
	}

	fmt.Printf("Chrome started (PID %d)\n", pid)
	fmt.Printf("Debug URL: %s\n", debugURL)
}

func cmdConnect(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney connect <host:port>")
	}
	hostport := args[0]
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		fatal("argument must be host:port (e.g. localhost:9222): %s", hostport)
	}

	// Fetch the WebSocket debugger URL from Chrome's /json/version endpoint
	resp, err := http.Get("http://" + hostport + "/json/version")
	if err != nil {
		fatal("could not reach browser at %s: %v", hostport, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fatal("failed to read response: %v", err)
	}
	var info struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &info); err != nil || info.WebSocketDebuggerURL == "" {
		fatal("unexpected response from browser at %s", hostport)
	}

	// Verify the connection works
	browser := rod.New().ControlURL(info.WebSocketDebuggerURL)
	if err := browser.Connect(); err != nil {
		fatal("could not connect to browser: %v", err)
	}

	// ChromePID=0 signals that we don't own this browser (stop won't kill it)
	state := &State{
		DebugURL:   info.WebSocketDebuggerURL,
		ChromePID:  0,
		ActivePage: 0,
	}
	if err := saveState(state); err != nil {
		fatal("failed to save state: %v", err)
	}

	fmt.Printf("Connected to browser at %s\n", hostport)
	fmt.Printf("Debug URL: %s\n", info.WebSocketDebuggerURL)
}

func cmdStop(args []string) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		// Try to kill by PID only if we launched the browser
		if s.ChromePID > 0 {
			proc, err := os.FindProcess(s.ChromePID)
			if err == nil {
				proc.Signal(syscall.SIGTERM)
			}
		}
	} else if s.ChromePID > 0 {
		// Only close (and kill) the browser if we launched it
		browser.MustClose()
	}
	// If ChromePID==0 we connected to an external browser; just clear state without closing it
	// Also kill the proxy helper if running
	if s.ProxyPID > 0 {
		if proc, err := os.FindProcess(s.ProxyPID); err == nil {
			proc.Signal(syscall.SIGTERM)
		}
	}
	// Kill the console collector if running and remove its buffer
	if s.ConsolePID > 0 {
		if proc, err := os.FindProcess(s.ConsolePID); err == nil {
			proc.Signal(syscall.SIGTERM)
		}
		if s.ConsoleLog != "" {
			os.Remove(s.ConsoleLog)
		}
	}
	// Clean up any active video recording
	if s.VideoRecording && s.VideoDir != "" {
		os.RemoveAll(s.VideoDir)
	}
	// Incognito sessions use a throwaway profile — remove it. Chrome may
	// still flush profile files during shutdown, so retry briefly.
	if s.Incognito && s.DataDir != "" {
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(s.DataDir); err == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		os.RemoveAll(s.DataDir)
	}
	removeState()
	fmt.Println("Chrome stopped")
}

func cmdStatus(args []string) {
	s, err := loadState()
	if err != nil {
		fmt.Println("No active browser session")
		return
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fmt.Printf("Browser not responding (PID %d, state may be stale)\n", s.ChromePID)
		return
	}
	pages, _ := browser.Pages()
	fmt.Printf("Browser running (PID %d)\n", s.ChromePID)
	fmt.Printf("Debug URL: %s\n", s.DebugURL)
	fmt.Printf("Pages: %d\n", len(pages))
	fmt.Printf("Active page: %d\n", s.ActivePage)
	if page, err := getActivePage(browser, s); err == nil {
		info, _ := page.Info()
		if info != nil {
			fmt.Printf("Current: %s - %s\n", info.Title, info.URL)
		}
	}
	if s.VideoRecording {
		frames := countFrames(s.VideoDir)
		fmt.Printf("Recording video (%d frames captured)\n", frames)
	}
}

// normalizeURL strips a single trailing slash (but keeps root "/") so
// example.com == example.com/ for --reuse matching.
func normalizeURL(u string) string {
	if len(u) > 0 && strings.HasSuffix(u, "/") && !strings.HasSuffix(u, "://") && u != "/" {
		return strings.TrimSuffix(u, "/")
	}
	return u
}

func cmdOpen(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney open <url> [--reuse]")
	}
	url := args[0]
	reuse := false
	for _, a := range args[1:] {
		switch a {
		case "--reuse":
			reuse = true
		default:
			fatal("unknown flag: %s (open <url> [--reuse])", a)
		}
	}
	// Add scheme if missing
	if !strings.Contains(url, "://") {
		url = "http://" + url
	}

	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	_ = browser // used below

	// --reuse: find an existing page already at this URL and switch to it
	// instead of navigating the active page away. Parallel sessions opening
	// the same URL converge on ONE page instead of opening it N times.
	// Trailing slash is normalized (example.com == example.com/).
	if reuse {
		if pages, err := browser.Pages(); err == nil {
			for i, p := range pages {
				if info, _ := p.Info(); info != nil && normalizeURL(info.URL) == normalizeURL(url) {
					s.ActivePage = i
					if err := saveState(s); err != nil {
						fatal("failed to save state: %v", err)
					}
					fmt.Printf("reuse: page [%d] %s\n", i, url)
					return
				}
			}
		}
	}

	// If no pages exist, create one
	pages, _ := browser.Pages()
	var page *rod.Page
	if len(pages) == 0 {
		page = browser.MustPage(url)
		s.ActivePage = 0
		saveState(s)
	} else {
		page, err = getActivePage(browser, s)
		if err != nil {
			fatal("%v", err)
		}
		// Re-apply session overrides (onload scripts must be registered
		// BEFORE the navigation so they run on the new document)
		applySessionOverrides(s, page)
		if err := page.Navigate(url); err != nil {
			fatal("navigation failed: %v", err)
		}
	}
	// Start video capture if recording is active
	videoCleanup = maybeStartVideoCapture(page)
	page.MustWaitLoad()
	info, _ := page.Info()
	if info != nil {
		fmt.Println(info.Title)
	}
}

func cmdBack(args []string) {
	_, _, page := withPage()
	page.MustNavigateBack()
	page.MustWaitLoad()
	info, _ := page.Info()
	if info != nil {
		fmt.Println(info.URL)
	}
}

func cmdForward(args []string) {
	_, _, page := withPage()
	page.MustNavigateForward()
	page.MustWaitLoad()
	info, _ := page.Info()
	if info != nil {
		fmt.Println(info.URL)
	}
}

func cmdReload(args []string) {
	fs := flag.NewFlagSet("reload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hard := fs.Bool("hard", false, "")
	fs.Parse(args)
	_, _, page := withPage()
	if *hard {
		// CDP Page.reload with ignoreCache (equivalent to Shift+Refresh)
		err := (proto.PageReload{IgnoreCache: true}).Call(page)
		if err != nil {
			fatal("reload failed: %v", err)
		}
	} else {
		page.MustReload()
	}
	page.MustWaitLoad()
	fmt.Println("Reloaded")
}

func cmdClearCache(args []string) {
	_, _, page := withPage()
	err := (proto.NetworkClearBrowserCache{}).Call(page)
	if err != nil {
		fatal("clear cache failed: %v", err)
	}
	fmt.Println("Browser cache cleared")
}

func cmdURL(args []string) {
	_, _, page := withPage()
	info, err := page.Info()
	if err != nil {
		fatal("failed to get page info: %v", err)
	}
	fmt.Println(info.URL)
}

func cmdTitle(args []string) {
	_, _, page := withPage()
	info, err := page.Info()
	if err != nil {
		fatal("failed to get page info: %v", err)
	}
	fmt.Println(info.Title)
}

func cmdHTML(args []string) {
	var rest []string
	for _, a := range args {
		if a == "--full" {
			_, _, page := withPage()
			res, err := page.Eval(`() => document.documentElement.outerHTML`)
			if err != nil {
				fatal("failed to get HTML: %v", err)
			}
			fmt.Println(res.Value.Str())
			// Append same-origin iframe contents (the outerHTML above shows
			// only the iframe tags, not their documents)
			n, err := page.Eval(`() => {
				const docs = [];
				for (const f of document.querySelectorAll('iframe')) {
					try { if (f.contentDocument) docs.push(f.contentDocument.documentElement.outerHTML); } catch (e) {}
				}
				return docs;
			}`)
			if err == nil {
				for _, d := range n.Value.Arr() {
					fmt.Printf("\n<!-- iframe content -->\n%v\n", d)
				}
			}
			return
		}
		rest = append(rest, a)
	}
	args = rest
	_, _, page := withPage()
	if len(args) > 0 {
		el, err := pageElShadow(page, args[0])
		if err != nil {
			fatal("element not found: %v", err)
		}
		html, err := el.HTML()
		if err != nil {
			fatal("failed to get HTML: %v", err)
		}
		fmt.Println(html)
	} else {
		html := page.MustEval(`() => document.documentElement.outerHTML`).Str()
		fmt.Println(html)
	}
}

func cmdText(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney text <selector>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	text, err := el.Text()
	if err != nil {
		fatal("failed to get text: %v", err)
	}
	fmt.Println(text)
}

func cmdAttr(args []string) {
	if len(args) < 2 {
		fatal("usage: rodney attr <selector> <attribute>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	val := el.MustAttribute(args[1])
	if val == nil {
		fatal("attribute %q not found", args[1])
	}
	fmt.Println(*val)
}

func cmdPDF(args []string) {
	var landscape bool
	var format string
	var margin float64
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--landscape":
			landscape = true
		case "--format":
			i++
			if i >= len(args) {
				fatal("--format requires a value (A4, Letter, Legal)")
			}
			format = args[i]
		case "--margin":
			i++
			if i >= len(args) {
				fatal("--margin requires a value in mm")
			}
			m, err := strconv.ParseFloat(args[i], 64)
			if err != nil || m < 0 {
				fatal("invalid margin: %s", args[i])
			}
			margin = m
		default:
			rest = append(rest, args[i])
		}
	}
	file := "page.pdf"
	if len(rest) > 0 {
		file = rest[0]
	}
	_, _, page := withPage()
	ptr := func(f float64) *float64 { return &f }
	req := proto.PagePrintToPDF{
		Landscape:    landscape,
		MarginTop:    ptr(margin / 25.4),
		MarginBottom: ptr(margin / 25.4),
		MarginLeft:   ptr(margin / 25.4),
		MarginRight:  ptr(margin / 25.4),
	}
	switch strings.ToLower(format) {
	case "":
	case "a4":
		req.PaperWidth, req.PaperHeight = ptr(8.27), ptr(11.69)
	case "letter":
		req.PaperWidth, req.PaperHeight = ptr(8.5), ptr(11)
	case "legal":
		req.PaperWidth, req.PaperHeight = ptr(8.5), ptr(14)
	default:
		fatal("unknown format %q (A4, Letter, Legal)", format)
	}
	r, err := page.PDF(&req)
	if err != nil {
		fatal("failed to generate PDF: %v", err)
	}
	buf := make([]byte, 0)
	tmp := make([]byte, 32*1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	if err := os.WriteFile(file, buf, 0644); err != nil {
		fatal("failed to write PDF: %v", err)
	}
	fmt.Printf("Saved %s (%d bytes)\n", file, len(buf))
}

func cmdJS(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney js <expression>")
	}
	expr := strings.Join(args, " ")
	_, _, page := withPage()

	// Wrap bare expressions in a function
	js := fmt.Sprintf(`() => { return (%s); }`, expr)
	result, err := page.Eval(js)
	if err != nil {
		fatal("JS error: %v", err)
	}
	// Print the value based on its JSON type
	v := result.Value
	raw := v.JSON("", "")
	// For simple types, print cleanly; for objects/arrays, pretty-print
	switch {
	case raw == "null" || raw == "undefined":
		fmt.Println(raw)
	case raw == "true" || raw == "false":
		fmt.Println(raw)
	case len(raw) > 0 && raw[0] == '"':
		// String value - print unquoted
		fmt.Println(v.Str())
	case len(raw) > 0 && (raw[0] == '{' || raw[0] == '['):
		// Object or array - pretty print
		fmt.Println(v.JSON("", "  "))
	default:
		// Numbers and other primitives
		fmt.Println(raw)
	}
}

// pageEl finds an element by CSS selector, or by XPath when the selector
// starts with "//" or "(" (the usual XPath conventions).
func pageEl(page *rod.Page, sel string) (*rod.Element, error) {
	if strings.HasPrefix(sel, "//") || strings.HasPrefix(sel, "(") {
		return page.ElementX(sel)
	}
	return page.Element(sel)
}

// globToRegex converts a simple glob (* = anything) to an anchored regex.
func globToRegex(glob string) string {
	escaped := regexp.QuoteMeta(glob)
	escaped = strings.ReplaceAll(escaped, "\\*", ".*")
	return "^" + escaped + "$"
}

// pageElDeep resolves "host >>> inner" chains through shadow roots
// (Playwright convention): each >>> segment descends one shadow boundary.
func pageElDeep(page *rod.Page, sel string) (*rod.Element, error) {
	parts := strings.Split(sel, ">>>")
	cur, err := pageEl(page, strings.TrimSpace(parts[0]))
	if err != nil {
		return nil, err
	}
	for _, part := range parts[1:] {
		root, err := cur.ShadowRoot()
		if err != nil {
			return nil, fmt.Errorf("no shadow root on %q: %w", parts[0], err)
		}
		cur, err = root.Element(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
	}
	return cur, nil
}

// pageElShadow routes "a >>> b" selectors through shadow roots; otherwise
// behaves like pageEl (CSS or XPath).
func pageElShadow(page *rod.Page, sel string) (*rod.Element, error) {
	if strings.Contains(sel, ">>>") {
		return pageElDeep(page, sel)
	}
	return pageEl(page, sel)
}

// pageEls is the plural variant of pageEl (XPath-aware).
func pageEls(page *rod.Page, sel string) (rod.Elements, error) {
	if strings.HasPrefix(sel, "//") || strings.HasPrefix(sel, "(") {
		return page.ElementsX(sel)
	}
	return page.Elements(sel)
}

func cmdClick(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney click <selector>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		fatal("click failed: %v", err)
	}
	// Brief pause for click handlers to execute
	time.Sleep(100 * time.Millisecond)
	fmt.Println("Clicked")
}

func cmdInput(args []string) {
	if len(args) < 2 {
		fatal("usage: rodney input <selector> <text>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	text := strings.Join(args[1:], " ")
	el.MustSelectAllText().MustInput(text)
	fmt.Printf("Typed: %s\n", text)
}

func cmdClear(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney clear <selector>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	el.MustSelectAllText().MustInput("")
	fmt.Println("Cleared")
}

func cmdFile(args []string) {
	if len(args) < 2 {
		fatal("usage: rodney file <selector> <path|->")
	}
	selector := args[0]
	filePath := args[1]

	_, _, page := withPage()
	el, err := page.Element(selector)
	if err != nil {
		fatal("element not found: %v", err)
	}

	if filePath == "-" {
		// Read from stdin to a temp file
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal("failed to read stdin: %v", err)
		}
		tmp, err := os.CreateTemp("", "rodney-upload-*")
		if err != nil {
			fatal("failed to create temp file: %v", err)
		}
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			fatal("failed to write temp file: %v", err)
		}
		tmp.Close()
		filePath = tmp.Name()
	} else {
		if _, err := os.Stat(filePath); err != nil {
			fatal("file not found: %v", err)
		}
	}

	if err := el.SetFiles([]string{filePath}); err != nil {
		fatal("failed to set file: %v", err)
	}
	fmt.Printf("Set file: %s\n", args[1])
}

func cmdDownload(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney download <selector> [file|-]")
	}
	selector := args[0]
	outFile := ""
	if len(args) > 1 {
		outFile = args[1]
	}

	_, _, page := withPage()
	el, err := page.Element(selector)
	if err != nil {
		fatal("element not found: %v", err)
	}

	// Get the URL from the element's href or src attribute
	urlStr := ""
	if v := el.MustAttribute("href"); v != nil {
		urlStr = *v
	} else if v := el.MustAttribute("src"); v != nil {
		urlStr = *v
	} else {
		fatal("element has no href or src attribute")
	}

	var data []byte

	if strings.HasPrefix(urlStr, "data:") {
		data, err = decodeDataURL(urlStr)
		if err != nil {
			fatal("failed to decode data URL: %v", err)
		}
	} else {
		// Use fetch() in the page context so it has cookies/session
		// Also resolves relative URLs automatically
		js := fmt.Sprintf(`async () => {
			const resp = await fetch(%q);
			if (!resp.ok) throw new Error('HTTP ' + resp.status);
			const buf = await resp.arrayBuffer();
			const bytes = new Uint8Array(buf);
			let binary = '';
			for (let i = 0; i < bytes.length; i++) {
				binary += String.fromCharCode(bytes[i]);
			}
			return btoa(binary);
		}`, urlStr)
		result, err := page.Eval(js)
		if err != nil {
			fatal("download failed: %v", err)
		}
		data, err = base64.StdEncoding.DecodeString(result.Value.Str())
		if err != nil {
			fatal("failed to decode response: %v", err)
		}
	}

	if outFile == "-" {
		os.Stdout.Write(data)
		return
	}

	if outFile == "" {
		outFile = inferDownloadFilename(urlStr)
	}

	if err := os.WriteFile(outFile, data, 0644); err != nil {
		fatal("failed to write file: %v", err)
	}
	fmt.Printf("Saved %s (%d bytes)\n", outFile, len(data))
}

// decodeDataURL decodes a data:[<mediatype>][;base64],<data> URL.
func decodeDataURL(dataURL string) ([]byte, error) {
	// Find the comma separating metadata from data
	commaIdx := strings.Index(dataURL, ",")
	if commaIdx < 0 {
		return nil, fmt.Errorf("invalid data URL: no comma found")
	}
	meta := dataURL[5:commaIdx] // skip "data:"
	encoded := dataURL[commaIdx+1:]

	if strings.HasSuffix(meta, ";base64") {
		return base64.StdEncoding.DecodeString(encoded)
	}
	// URL-encoded text
	decoded, err := url.QueryUnescape(encoded)
	if err != nil {
		return nil, err
	}
	return []byte(decoded), nil
}

// inferDownloadFilename tries to extract a reasonable filename from a URL.
func inferDownloadFilename(urlStr string) string {
	if strings.HasPrefix(urlStr, "data:") {
		// Extract MIME type for extension
		commaIdx := strings.Index(urlStr, ",")
		if commaIdx > 0 {
			meta := urlStr[5:commaIdx]
			meta = strings.TrimSuffix(meta, ";base64")
			ext := mimeToExt(meta)
			return nextAvailableFile("download", ext)
		}
		return nextAvailableFile("download", "")
	}

	parsed, err := url.Parse(urlStr)
	if err == nil && parsed.Path != "" && parsed.Path != "/" {
		base := filepath.Base(parsed.Path)
		if base != "." && base != "/" {
			return nextAvailableFile(
				strings.TrimSuffix(base, filepath.Ext(base)),
				filepath.Ext(base),
			)
		}
	}
	return nextAvailableFile("download", "")
}

// cmdMock intercepts requests matching a URL pattern and serves a canned
// response instead of hitting the real server. It runs as a persistent
// foreground command: the interception router stays alive until the process
// is interrupted (Ctrl+C / SIGTERM), so other rodney commands in separate
// shells can drive the browser while the mock is active.
//
// Usage:
//
//	rodney mock <pattern> <response> [--status N] [--type MIME] [--method M]
//
// pattern is a glob-style URL pattern (e.g. "*api.example.com/users*"),
// response is the body text to serve (or "-file=<path>" to read a file),
// and optional flags set the HTTP status, Content-Type, and request method
// to match. If --method is given, only requests with that method are mocked.
func cmdMock(args []string) {
	fs := flag.NewFlagSet("mock", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	status := fs.Int("status", 200, "")
	contentType := fs.String("type", "text/plain", "")
	method := fs.String("method", "", "")

	if err := fs.Parse(args); err != nil {
		fatal("%v", err)
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fatal("usage: rodney mock <pattern> <response> [--status N] [--type MIME] [--method M]")
	}
	pattern := rest[0]
	body := rest[1]

	if strings.HasPrefix(body, "-file=") {
		data, err := os.ReadFile(strings.TrimPrefix(body, "-file="))
		if err != nil {
			fatal("failed to read response file: %v", err)
		}
		body = string(data)
	}

	_, _, page := withPage()
	router := page.HijackRequests()

	if err := router.Add(pattern, "", func(h *rod.Hijack) {
		if *method != "" && h.Request.Method() != *method {
			h.ContinueRequest(&proto.FetchContinueRequest{})
			return
		}
		h.Response.SetHeader("Content-Type", *contentType).SetBody(body)
		h.Response.Payload().ResponseCode = *status
	}); err != nil {
		fatal("failed to install mock: %v", err)
	}

	router.Run()
	defer func() { _ = router.Stop() }()

	fmt.Printf("Mocking %s -> %d %s (Ctrl+C to stop)\n", pattern, *status, *contentType)

	// Keep the router alive until interrupted.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

// cmdBlock intercepts requests matching a URL pattern and fails them client-
// side, so the browser never reaches the real server. Like cmdMock it runs as
// a persistent foreground command until interrupted.
//
// Usage:
//
//	rodney block <pattern> [--method M]
func cmdBlock(args []string) {
	fs := flag.NewFlagSet("block", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	method := fs.String("method", "", "")

	if err := fs.Parse(args); err != nil {
		fatal("%v", err)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fatal("usage: rodney block <pattern> [--method M]")
	}
	pattern := rest[0]

	_, _, page := withPage()
	router := page.HijackRequests()

	if err := router.Add(pattern, "", func(h *rod.Hijack) {
		if *method != "" && h.Request.Method() != *method {
			h.ContinueRequest(&proto.FetchContinueRequest{})
			return
		}
		h.Response.Fail(proto.NetworkErrorReasonBlockedByClient)
	}); err != nil {
		fatal("failed to install blocker: %v", err)
	}

	router.Run()
	defer func() { _ = router.Stop() }()

	fmt.Printf("Blocking %s (Ctrl+C to stop)\n", pattern)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

// mimeToExt returns a file extension for common MIME types.
func mimeToExt(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "text/html":
		return ".html"
	case "text/css":
		return ".css"
	case "application/json":
		return ".json"
	case "application/javascript":
		return ".js"
	case "application/octet-stream":
		return ".bin"
	default:
		return ""
	}
}

func cmdSelect(args []string) {
	if len(args) < 2 {
		fatal("usage: rodney select <selector> <value>")
	}
	_, _, page := withPage()
	// Use JavaScript to set the value, as rod's Select matches by text
	js := fmt.Sprintf(`() => {
		const el = document.querySelector(%q);
		if (!el) throw new Error('element not found');
		el.value = %q;
		el.dispatchEvent(new Event('change', {bubbles: true}));
		return el.value;
	}`, args[0], args[1])
	result, err := page.Eval(js)
	if err != nil {
		fatal("select failed: %v", err)
	}
	fmt.Printf("Selected: %s\n", result.Value.Str())
}

func cmdSubmit(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney submit <selector>")
	}
	_, _, page := withPage()
	_, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("form not found: %v", err)
	}
	page.MustEval(fmt.Sprintf(`() => document.querySelector(%q).submit()`, args[0]))
	fmt.Println("Submitted")
}

func cmdHover(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney hover <selector>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	el.MustHover()
	fmt.Println("Hovered")
}

func cmdFocus(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney focus <selector>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	el.MustFocus()
	fmt.Println("Focused")
}

// keyNames maps friendly key names to rod input.Key constants.
// Names are case-insensitive; unknown names are an error.
func keyNames() map[string]input.Key {
	return map[string]input.Key{
		"enter": input.Enter, "return": input.Enter,
		"tab":    input.Tab,
		"escape": input.Escape, "esc": input.Escape,
		"backspace": input.Backspace,
		"delete":    input.Delete, "del": input.Delete,
		"space": input.Space,
		"up":    input.ArrowUp, "down": input.ArrowDown,
		"left": input.ArrowLeft, "right": input.ArrowRight,
		"home": input.Home, "end": input.End,
		"pageup": input.PageUp, "pagedown": input.PageDown,
		"shift": input.ShiftLeft, "ctrl": input.ControlLeft, "control": input.ControlLeft,
		"alt": input.AltLeft, "meta": input.MetaLeft, "cmd": input.MetaLeft,
	}
}

// parseKeyNames converts key names (comma- or space-separated args) to input.Keys.
// A single character like "a" maps to its key; "ctrl+enter" style combos are
// expanded into press-all-then-release-all sequences by the caller.
func parseKeyNames(names []string) ([]input.Key, error) {
	var keys []input.Key
	for _, name := range names {
		for _, part := range strings.Split(name, "+") {
			part = strings.TrimSpace(strings.ToLower(part))
			if part == "" {
				continue
			}
			if k, ok := keyNames()[part]; ok {
				keys = append(keys, k)
				continue
			}
			// Single character (letter, digit, punctuation) — Key is a rune,
			// printable chars map directly
			if len([]rune(part)) == 1 {
				keys = append(keys, input.Key([]rune(part)[0]))
				continue
			}
			return nil, fmt.Errorf("unknown key %q (try: enter, tab, escape, backspace, delete, space, up, down, left, right, or a single character)", part)
		}
	}
	return keys, nil
}

// cmdPress presses keys as real keyboard events (keydown+keyup), e.g.// "rodney press enter", "rodney press ctrl+a", "rodney press shift tab".
// Unlike `input`, this fires real key events — SPA listeners and form
// validation react to it.
func cmdPress(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney press <key> [key ...]  (e.g. enter, tab, ctrl+a)")
	}
	keys, err := parseKeyNames(args)
	if err != nil {
		fatal("%v", err)
	}
	_, _, page := withPage()
	ka := page.KeyActions().Press(keys...)
	if err := ka.Do(); err != nil {
		fatal("key press failed: %v", err)
	}
	fmt.Printf("Pressed: %s\n", strings.Join(args, " "))
}

// cmdType types text as real keyboard input into the focused element via
// Input.insertText — fires input events (unlike `input`, which sets .value
// directly without events).
func cmdType(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney type <text>")
	}
	text := strings.Join(args, " ")
	_, _, page := withPage()
	if err := page.InsertText(text); err != nil {
		fatal("type failed: %v", err)
	}
	fmt.Printf("Typed: %s\n", text)
}

// cmdScroll scrolls the page by (x, y) pixels — negative y scrolls up.
// --steps N scrolls in N increments (smooth scrolling for lazy-loading).
func cmdScroll(args []string) {
	var steps int
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--steps":
			i++
			if i >= len(args) {
				fatal("--steps requires a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 {
				fatal("--steps must be a positive integer")
			}
			steps = n
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) != 2 {
		fatal("usage: rodney scroll <x> <y> [--steps N]  (negative y scrolls up)")
	}
	x, err := strconv.ParseFloat(rest[0], 64)
	if err != nil {
		fatal("invalid x: %v", err)
	}
	y, err := strconv.ParseFloat(rest[1], 64)
	if err != nil {
		fatal("invalid y: %v", err)
	}
	if steps == 0 {
		steps = 1
	}
	_, _, page := withPage()
	if err := page.Mouse.Scroll(x, y, steps); err != nil {
		fatal("scroll failed: %v", err)
	}
	fmt.Printf("Scrolled (%.0f, %.0f)\n", x, y)
}

// cmdScrollEl scrolls an element into view.
func cmdScrollEl(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney scroll-el <selector>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	if err := el.ScrollIntoView(); err != nil {
		fatal("scroll into view failed: %v", err)
	}
	fmt.Println("Scrolled element into view")
}

func cmdWait(args []string) {
	// wait --url <substring>: wait until location.href contains substring
	if len(args) >= 2 && args[0] == "--url" {
		_, _, page := withPage()
		url, err := waitURLContains(page, args[1], defaultTimeout)
		if err != nil {
			fatal("timeout waiting for URL containing %q", args[1])
		}
		fmt.Println(url)
		return
	}
	if len(args) < 1 {
		fatal("usage: rodney wait <selector> | rodney wait --url <substring>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	el.MustWaitVisible()
	fmt.Println("Element visible")
}

func cmdWaitLoad(args []string) {
	_, _, page := withPage()
	page.MustWaitLoad()
	fmt.Println("Page loaded")
}

func cmdWaitStable(args []string) {
	_, _, page := withPage()
	page.MustWaitStable()
	fmt.Println("DOM stable")
}

func cmdWaitIdle(args []string) {
	var includes, excludes []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--include", "--exclude":
			i++
			if i >= len(args) {
				fatal("%s requires a comma-separated pattern list", args[i-1])
			}
			for _, p := range strings.Split(args[i], ",") {
				if p == "" {
					continue
				}
				if args[i-1] == "--include" {
					includes = append(includes, globToRegex(p))
				} else {
					excludes = append(excludes, globToRegex(p))
				}
			}
		default:
			fatal("unknown flag: %s\nusage: rodney waitidle [--include p1,p2] [--exclude p1,p2]", args[i])
		}
	}
	_, _, page := withPage()
	if len(includes) > 0 || len(excludes) > 0 {
		wait := page.WaitRequestIdle(time.Second, includes, excludes, nil)
		wait()
		fmt.Println("Network idle (filtered)")
		return
	}
	page.MustWaitIdle()
	fmt.Println("Network idle")
}

func cmdSleep(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney sleep <seconds>")
	}
	secs, err := strconv.ParseFloat(args[0], 64)
	if err != nil {
		fatal("invalid seconds: %v", err)
	}
	// If video recording is active, connect to the page so screencast
	// captures frames during the sleep
	if s, err := loadState(); err == nil && s.VideoRecording {
		withPage()
	}
	time.Sleep(time.Duration(secs * float64(time.Second)))
}

// nextAvailableFile returns "base+ext" if it doesn't exist,
// otherwise "base-2+ext", "base-3+ext", etc.
func nextAvailableFile(base, ext string) string {
	name := base + ext
	if _, err := os.Stat(name); os.IsNotExist(err) {
		return name
	}
	for i := 2; ; i++ {
		name = fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(name); os.IsNotExist(err) {
			return name
		}
	}
}

func cmdScreenshot(args []string) {
	fs := flag.NewFlagSet("screenshot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	width := fs.Int("width", 1280, "")
	fs.IntVar(width, "w", 1280, "")
	height := fs.Int("height", 0, "")
	fs.IntVar(height, "h", 0, "")
	fs.Bool("full", false, "")

	if err := fs.Parse(args); err != nil {
		fatal("%v", err)
	}

	fullPage := true
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "height" || f.Name == "h" {
			fullPage = false
		}
		if f.Name == "full" {
			fullPage = true
		}
	})

	var file string
	if fs.NArg() > 0 {
		file = fs.Arg(0)
	} else {
		file = nextAvailableFile("screenshot", ".png")
	}

	_, _, page := withPage()

	// Set viewport size
	viewportHeight := *height
	if viewportHeight == 0 {
		viewportHeight = 720
	}
	err := proto.EmulationSetDeviceMetricsOverride{
		Width:             *width,
		Height:            viewportHeight,
		DeviceScaleFactor: 1,
	}.Call(page)
	if err != nil {
		fatal("failed to set viewport: %v", err)
	}

	data, err := page.Screenshot(fullPage, nil)
	if err != nil {
		fatal("screenshot failed: %v", err)
	}
	if err := os.WriteFile(file, data, 0644); err != nil {
		fatal("failed to write screenshot: %v", err)
	}
	fmt.Println(file)
}

func cmdScreenshotEl(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney screenshot-el <selector> [file]")
	}
	file := "element.png"
	if len(args) > 1 {
		file = args[1]
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	data, err := el.Screenshot(proto.PageCaptureScreenshotFormatPng, 0)
	if err != nil {
		fatal("screenshot failed: %v", err)
	}
	if err := os.WriteFile(file, data, 0644); err != nil {
		fatal("failed to write screenshot: %v", err)
	}
	fmt.Printf("Saved %s (%d bytes)\n", file, len(data))
}

// --- Video recording ---

// startVideo enables video recording: sets state flag and creates frames dir.
func startVideo() error {
	s, err := loadState()
	if err != nil {
		return err
	}
	if s.VideoRecording {
		return fmt.Errorf("video recording already in progress")
	}
	s.VideoDir = filepath.Join(stateDir(), "video-frames")
	if err := os.MkdirAll(s.VideoDir, 0755); err != nil {
		return fmt.Errorf("failed to create video dir: %w", err)
	}
	s.VideoRecording = true
	return saveState(s)
}

func cmdStartVideo(args []string) {
	if err := startVideo(); err != nil {
		fatal("%v", err)
	}
	fmt.Println("Video recording started")
}

// VideoResult holds the result of stop-video.
type VideoResult struct {
	FrameCount     int
	UniqueFrames   int    // for GIF: frames after deduplication
	OutputFile     string // empty if assembly failed
	FallbackFormat bool   // true if fell back to GIF because ffmpeg was unavailable
}

// stopVideo stops recording, optionally assembles video, clears state.
func stopVideo(outputFile string) (*VideoResult, error) {
	s, err := loadState()
	if err != nil {
		return nil, err
	}
	if !s.VideoRecording {
		return nil, fmt.Errorf("video recording is not active (run 'rodney start-video' first)")
	}

	framesDir := s.VideoDir
	frameCount := countFrames(framesDir)

	result := &VideoResult{FrameCount: frameCount}

	// Assemble output if we have frames
	if frameCount > 0 && outputFile != "" {
		if strings.HasSuffix(strings.ToLower(outputFile), ".gif") {
			gifResult, err := assembleGIF(framesDir, outputFile)
			if err != nil {
				return nil, fmt.Errorf("GIF assembly failed: %w", err)
			}
			result.OutputFile = gifResult.OutputFile
			result.UniqueFrames = gifResult.UniqueFrames
		} else {
			assembled, err := assembleVideo(framesDir, outputFile)
			if err == nil {
				result.OutputFile = assembled
			} else {
				// ffmpeg not available — fall back to GIF
				gifFile := strings.TrimSuffix(outputFile, filepath.Ext(outputFile)) + ".gif"
				if gifResult, gifErr := assembleGIF(framesDir, gifFile); gifErr == nil {
					result.OutputFile = gifResult.OutputFile
					result.UniqueFrames = gifResult.UniqueFrames
					result.FallbackFormat = true
				}
			}
		}
	}

	// Clean up: remove frames dir
	os.RemoveAll(framesDir)

	// Clear state
	s.VideoRecording = false
	s.VideoDir = ""
	saveState(s)

	return result, nil
}

func cmdStopVideo(args []string) {
	outputFile := ""
	if len(args) > 0 {
		outputFile = args[0]
	} else {
		outputFile = nextAvailableFile("recording", ".gif")
	}

	result, err := stopVideo(outputFile)
	if err != nil {
		fatal("%v", err)
	}

	if result.OutputFile != "" {
		if result.FallbackFormat {
			fmt.Fprintf(os.Stderr, "ffmpeg not found, saving as GIF instead\n")
		}
		if result.UniqueFrames > 0 && result.UniqueFrames < result.FrameCount {
			fmt.Printf("Saved %s (%d frames, %d unique)\n", result.OutputFile, result.FrameCount, result.UniqueFrames)
		} else {
			fmt.Printf("Saved %s (%d frames)\n", result.OutputFile, result.FrameCount)
		}
	} else if result.FrameCount > 0 {
		fmt.Printf("Captured %d frames but assembly failed\n", result.FrameCount)
	} else {
		fmt.Println("No frames captured")
	}
}

// assembleVideo uses ffmpeg to combine frames into an MP4 video.
// Returns the output file path on success.
func assembleVideo(framesDir, outputFile string) (string, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg not found: %w", err)
	}

	// Read metadata for variable frame timing
	metaPath := filepath.Join(framesDir, "meta.jsonl")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		// Fallback: use constant framerate
		return assembleConstantFPS(ffmpeg, framesDir, outputFile)
	}

	return assembleVariableFPS(ffmpeg, framesDir, outputFile, metaData)
}

// assembleConstantFPS assembles frames at a fixed 10fps.
func assembleConstantFPS(ffmpeg, framesDir, outputFile string) (string, error) {
	cmd := exec.Command(ffmpeg, "-y",
		"-framerate", "10",
		"-i", filepath.Join(framesDir, "frame_%06d.jpeg"),
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-preset", "fast",
		outputFile,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg failed: %v: %s", err, output)
	}
	return outputFile, nil
}

// assembleVariableFPS uses ffmpeg concat demuxer with per-frame durations from metadata.
func assembleVariableFPS(ffmpeg, framesDir, outputFile string, metaData []byte) (string, error) {
	type frameMeta struct {
		Idx int     `json:"idx"`
		Ts  float64 `json:"ts"`
	}

	lines := strings.Split(strings.TrimSpace(string(metaData)), "\n")
	var frames []frameMeta
	for _, line := range lines {
		var fm frameMeta
		if err := json.Unmarshal([]byte(line), &fm); err == nil {
			frames = append(frames, fm)
		}
	}

	if len(frames) < 2 {
		return assembleConstantFPS(ffmpeg, framesDir, outputFile)
	}

	// Write concat demuxer file
	concatPath := filepath.Join(framesDir, "concat.txt")
	f, err := os.Create(concatPath)
	if err != nil {
		return assembleConstantFPS(ffmpeg, framesDir, outputFile)
	}
	for i, fm := range frames {
		framePath := filepath.Join(framesDir, fmt.Sprintf("frame_%06d.jpeg", fm.Idx))
		fmt.Fprintf(f, "file '%s'\n", framePath)
		if i < len(frames)-1 {
			dur := frames[i+1].Ts - fm.Ts
			if dur <= 0 {
				dur = 0.033
			}
			fmt.Fprintf(f, "duration %.6f\n", dur)
		} else {
			fmt.Fprintf(f, "duration 0.033\n")
		}
	}
	f.Close()

	cmd := exec.Command(ffmpeg, "-y",
		"-f", "concat", "-safe", "0",
		"-i", concatPath,
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-preset", "fast",
		outputFile,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg failed: %v: %s", err, output)
	}
	return outputFile, nil
}

// GIFResult holds stats from GIF assembly.
type GIFResult struct {
	OutputFile   string
	InputFrames  int
	UniqueFrames int
}

// assembleGIF creates an animated GIF from JPEG frames with frame deduplication.
// Identical consecutive frames are merged into a single frame with extended duration.
func assembleGIF(framesDir, outputFile string) (*GIFResult, error) {
	// Read metadata for frame timing
	metaPath := filepath.Join(framesDir, "meta.jsonl")
	metaData, _ := os.ReadFile(metaPath)

	type frameMeta struct {
		Idx int     `json:"idx"`
		Ts  float64 `json:"ts"`
	}
	var metas []frameMeta
	if len(metaData) > 0 {
		for _, line := range strings.Split(strings.TrimSpace(string(metaData)), "\n") {
			var fm frameMeta
			if json.Unmarshal([]byte(line), &fm) == nil {
				metas = append(metas, fm)
			}
		}
	}

	// List frame files in order
	entries, err := os.ReadDir(framesDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read frames dir: %w", err)
	}
	var frameFiles []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "frame_") && strings.HasSuffix(e.Name(), ".jpeg") {
			frameFiles = append(frameFiles, filepath.Join(framesDir, e.Name()))
		}
	}
	sort.Strings(frameFiles)

	if len(frameFiles) == 0 {
		return nil, fmt.Errorf("no frames to assemble")
	}

	// Build timing lookup: index -> duration in centiseconds (1/100 sec)
	frameDurations := make(map[int]int) // frame index -> delay in centiseconds
	for i := 0; i < len(metas)-1; i++ {
		dur := metas[i+1].Ts - metas[i].Ts
		if dur <= 0 {
			dur = 0.033
		}
		cs := int(dur*100 + 0.5) // convert to centiseconds, rounded
		if cs < 2 {
			cs = 2 // GIF minimum delay is 2cs (20ms) in most viewers
		}
		frameDurations[metas[i].Idx] = cs
	}

	// Process frames: decode JPEG, quantize to paletted, deduplicate
	pal := palette.Plan9
	var gifImages []*image.Paletted
	var gifDelays []int
	var prevPix []byte
	inputCount := len(frameFiles)

	for i, path := range frameFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			continue
		}

		// Quantize to 256-color paletted image
		bounds := img.Bounds()
		paletted := image.NewPaletted(bounds, pal)
		draw.FloydSteinberg.Draw(paletted, bounds, img, image.Point{})

		// Determine this frame's duration
		delay := 3 // default 30ms
		// Extract index from filename for metadata lookup
		base := filepath.Base(path)
		if idx, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(base, "frame_"), ".jpeg")); err == nil {
			if d, ok := frameDurations[idx]; ok {
				delay = d
			}
		}
		// Last frame gets default delay if not in metadata
		if i == len(frameFiles)-1 && delay == 3 {
			delay = 10 // 100ms for last frame
		}

		// Deduplicate: compare paletted pixels
		if prevPix != nil && bytes.Equal(paletted.Pix, prevPix) {
			// Same as previous frame — extend its delay
			gifDelays[len(gifDelays)-1] += delay
		} else {
			gifImages = append(gifImages, paletted)
			gifDelays = append(gifDelays, delay)
			prevPix = make([]byte, len(paletted.Pix))
			copy(prevPix, paletted.Pix)
		}
	}

	if len(gifImages) == 0 {
		return nil, fmt.Errorf("no valid frames decoded")
	}

	// Write GIF
	f, err := os.Create(outputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}
	defer f.Close()

	err = gif.EncodeAll(f, &gif.GIF{
		Image:     gifImages,
		Delay:     gifDelays,
		LoopCount: 0, // loop forever
	})
	if err != nil {
		return nil, fmt.Errorf("GIF encoding failed: %w", err)
	}

	return &GIFResult{
		OutputFile:   outputFile,
		InputFrames:  inputCount,
		UniqueFrames: len(gifImages),
	}, nil
}

// startVideoCapture begins CDP screencast on the given page, writing JPEG frames
// and metadata to framesDir. It returns a stop function that stops the screencast
// and returns the number of frames captured in this session.
func startVideoCapture(page *rod.Page, framesDir string) (stop func() int) {
	os.MkdirAll(framesDir, 0755)

	// Count existing frames to continue numbering
	startIdx := countFrames(framesDir)

	var mu sync.Mutex
	captured := 0

	// Open metadata file for appending
	metaPath := filepath.Join(framesDir, "meta.jsonl")
	metaFile, err := os.OpenFile(metaPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		// Non-fatal: we can still capture frames without metadata
		metaFile = nil
	}

	done := make(chan struct{})

	go page.EachEvent(func(e *proto.PageScreencastFrame) bool {
		select {
		case <-done:
			return true
		default:
		}

		mu.Lock()
		idx := startIdx + captured
		captured++
		mu.Unlock()

		framePath := filepath.Join(framesDir, fmt.Sprintf("frame_%06d.jpeg", idx))
		os.WriteFile(framePath, e.Data, 0644)

		if metaFile != nil && e.Metadata != nil {
			line := fmt.Sprintf(`{"idx":%d,"ts":%.6f}`+"\n", idx, float64(e.Metadata.Timestamp))
			mu.Lock()
			metaFile.WriteString(line)
			mu.Unlock()
		}

		proto.PageScreencastFrameAck{SessionID: e.SessionID}.Call(page)
		return false
	})()

	quality := 80
	everyNth := 1
	proto.PageStartScreencast{
		Format:        proto.PageStartScreencastFormatJpeg,
		Quality:       &quality,
		EveryNthFrame: &everyNth,
	}.Call(page)

	return func() int {
		proto.PageStopScreencast{}.Call(page)
		close(done)
		// Give in-flight frames a moment to flush
		time.Sleep(50 * time.Millisecond)
		if metaFile != nil {
			metaFile.Close()
		}
		mu.Lock()
		defer mu.Unlock()
		return captured
	}
}

// countFrames counts existing frame_NNNNNN.jpeg files in a directory.
func countFrames(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "frame_") && strings.HasSuffix(e.Name(), ".jpeg") {
			n++
		}
	}
	return n
}

func cmdPages(args []string) {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		default:
			fatal("unknown flag: %s\nusage: rodney pages [--json]", a)
		}
	}
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	pages, err := browser.Pages()
	if err != nil {
		fatal("failed to list pages: %v", err)
	}
	if asJSON {
		type pageInfo struct {
			Index  int    `json:"index"`
			Target string `json:"target"` // stable target ID, usable as `page t:<id>` / `closepage t:<id>`
			Title  string `json:"title"`
			URL    string `json:"url"`
			Active bool   `json:"active"`
		}
		list := make([]pageInfo, 0, len(pages))
		for i, p := range pages {
			pi := pageInfo{Index: i, Target: string(p.TargetID), Active: i == s.ActivePage}
			if info, _ := p.Info(); info != nil {
				pi.Title = info.Title
				pi.URL = info.URL
			}
			list = append(list, pi)
		}
		b, err := json.MarshalIndent(list, "", "  ")
		if err != nil {
			fatal("failed to marshal pages: %v", err)
		}
		fmt.Println(string(b))
		return
	}
	for i, p := range pages {
		marker := " "
		if i == s.ActivePage {
			marker = "*"
		}
		info, _ := p.Info()
		if info != nil {
			fmt.Printf("%s [%d] (t:%s) %s - %s\n", marker, i, p.TargetID, info.Title, info.URL)
		} else {
			fmt.Printf("%s [%d] (t:%s) (unknown)\n", marker, i, p.TargetID)
		}
	}
}

func cmdPage(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney page <index|t:targetID>")
	}
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	pages, err := browser.Pages()
	if err != nil {
		fatal("failed to list pages: %v", err)
	}
	// t:<targetID>: pin by the page's STABLE target ID — index-drift-proof for
	// parallel sessions (another session opening a page shifts every index).
	if strings.HasPrefix(args[0], "t:") {
		tid := proto.TargetTargetID(strings.TrimPrefix(args[0], "t:"))
		for i, p := range pages {
			if p.TargetID == tid {
				s.ActivePage = i
				if err := saveState(s); err != nil {
					fatal("failed to save state: %v", err)
				}
				info, _ := pages[i].Info()
				if info != nil {
					fmt.Printf("Switched to [%d] %s - %s\n", i, info.Title, info.URL)
				}
				return
			}
		}
		fatal("no page with target ID %s (list with: rodney pages)", tid)
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil {
		fatal("invalid index: %v", err)
	}
	if idx < 0 || idx >= len(pages) {
		fatal("page index %d out of range (0-%d)", idx, len(pages)-1)
	}
	s.ActivePage = idx
	if err := saveState(s); err != nil {
		fatal("failed to save state: %v", err)
	}
	info, _ := pages[idx].Info()
	if info != nil {
		fmt.Printf("Switched to [%d] %s - %s\n", idx, info.Title, info.URL)
	}
}

func cmdNewPage(args []string) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}

	url := ""
	if len(args) > 0 {
		url = args[0]
		if !strings.Contains(url, "://") {
			url = "http://" + url
		}
	}

	var page *rod.Page
	if url != "" {
		page = browser.MustPage(url)
		page.MustWaitLoad()
	} else {
		page = browser.MustPage("")
	}

	// Switch active to the new page
	pages, _ := browser.Pages()
	for i, p := range pages {
		if p.TargetID == page.TargetID {
			s.ActivePage = i
			break
		}
	}
	saveState(s)

	info, _ := page.Info()
	if info != nil {
		fmt.Printf("Opened [%d] %s\n", s.ActivePage, info.URL)
	}
}

func cmdClosePage(args []string) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	pages, err := browser.Pages()
	if err != nil {
		fatal("failed to list pages: %v", err)
	}
	if len(pages) <= 1 {
		fatal("cannot close the last page")
	}

	idx := s.ActivePage
	if len(args) > 0 {
		// t:<targetID>: close by the page's STABLE target ID — drift-proof for
		// parallel sessions (indices shift when another session opens/closes pages).
		if strings.HasPrefix(args[0], "t:") {
			tid := proto.TargetTargetID(strings.TrimPrefix(args[0], "t:"))
			found := -1
			for i, p := range pages {
				if p.TargetID == tid {
					found = i
					break
				}
			}
			if found < 0 {
				fatal("no page with target ID %s (list with: rodney pages)", tid)
			}
			idx = found
		} else {
			idx, err = strconv.Atoi(args[0])
			if err != nil {
				fatal("invalid index: %v", err)
			}
		}
	}
	if idx < 0 || idx >= len(pages) {
		fatal("page index %d out of range", idx)
	}

	closedTarget := pages[idx].TargetID
	pages[idx].MustClose()

	// Adjust active page: if we closed the active page, fall back to an
	// adjacent one; if the active index shifted past the end, clamp it.
	if s.ActivePage == idx {
		if s.ActivePage >= len(pages)-1 {
			s.ActivePage = len(pages) - 2
		}
		if s.ActivePage < 0 {
			s.ActivePage = 0
		}
	} else if s.ActivePage > idx {
		// Active page was after the closed one — its index shifts down by one
		s.ActivePage--
	}
	saveState(s)
	fmt.Printf("Closed page %d (t:%s)\n", idx, closedTarget)
}

func cmdExists(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney exists <selector>")
	}
	_, _, page := withPage()
	var has bool
	var err error
	if strings.HasPrefix(args[0], "//") || strings.HasPrefix(args[0], "(") {
		els, e := pageEls(page, args[0])
		err = e
		has = len(els) > 0
	} else {
		has, _, err = page.Has(args[0])
	}
	if err != nil {
		fatal("query failed: %v", err)
	}
	if has {
		fmt.Println("true")
		os.Exit(0)
	} else {
		fmt.Println("false")
		os.Exit(1)
	}
}

func cmdCount(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney count <selector>")
	}
	_, _, page := withPage()
	els, err := pageEls(page, args[0])
	if err != nil {
		fatal("query failed: %v", err)
	}
	fmt.Println(len(els))
}

func cmdVisible(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney visible <selector>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fmt.Println("false")
		os.Exit(1)
	}
	visible, err := el.Visible()
	if err != nil {
		fmt.Println("false")
		os.Exit(1)
	}
	if visible {
		fmt.Println("true")
		os.Exit(0)
	} else {
		fmt.Println("false")
		os.Exit(1)
	}
}

// parseAssertArgs separates flags (--message/-m) from positional args.
// Returns (expression, expected, message). expected is nil for truthy mode.
func parseAssertArgs(args []string) (expr string, expected *string, message string) {
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--message", "-m":
			i++
			if i < len(args) {
				message = args[i]
			}
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) >= 1 {
		expr = positional[0]
	}
	if len(positional) >= 2 {
		expected = &positional[1]
	}
	return
}

// formatAssertFail builds the failure output line.
// For truthy failures expected is nil; for equality failures it points to the expected string.
func formatAssertFail(actual string, expected *string, message string) string {
	if expected != nil {
		// Equality mode
		detail := fmt.Sprintf("got %q, expected %q", actual, *expected)
		if message != "" {
			return fmt.Sprintf("fail: %s (%s)", message, detail)
		}
		return fmt.Sprintf("fail: %s", detail)
	}
	// Truthy mode
	if message != "" {
		return fmt.Sprintf("fail: %s (got %s)", message, actual)
	}
	return fmt.Sprintf("fail: got %s", actual)
}

func cmdAssert(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney assert <js-expression> [expected] [--message msg]")
	}

	expr, expected, message := parseAssertArgs(args)
	if expr == "" {
		fatal("usage: rodney assert <js-expression> [expected] [--message msg]")
	}

	_, _, page := withPage()

	js := fmt.Sprintf(`() => { return (%s); }`, expr)
	result, err := page.Eval(js)
	if err != nil {
		fatal("JS error: %v", err)
	}

	// Format the result value as a string, matching the js command's output
	v := result.Value
	raw := v.JSON("", "")
	var actual string
	switch {
	case raw == "null" || raw == "undefined":
		actual = raw
	case raw == "true" || raw == "false":
		actual = raw
	case len(raw) > 0 && raw[0] == '"':
		actual = v.Str()
	case len(raw) > 0 && (raw[0] == '{' || raw[0] == '['):
		actual = v.JSON("", "  ")
	default:
		actual = raw
	}

	if expected != nil {
		// Equality mode: compare string representation to expected
		if actual == *expected {
			fmt.Println("pass")
			os.Exit(0)
		} else {
			fmt.Println(formatAssertFail(actual, expected, message))
			os.Exit(1)
		}
	} else {
		// Truthy mode: check if the JS value is truthy
		switch raw {
		case "false", "0", "null", "undefined", `""`:
			fmt.Println(formatAssertFail(actual, nil, message))
			os.Exit(1)
		default:
			fmt.Println("pass")
			os.Exit(0)
		}
	}
}

// Ignore SIGPIPE for piped output
func init() {
	signal.Ignore(syscall.SIGPIPE)
}

// --- Accessibility commands ---

func cmdAXTree(args []string) {
	fs := flag.NewFlagSet("ax-tree", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	depthVal := fs.Int("depth", 0, "")
	jsonOutput := fs.Bool("json", false, "")

	if err := fs.Parse(args); err != nil {
		fatal("unknown flag: %s\nusage: rodney ax-tree [--depth N] [--json]", findUnknownFlag(args, fs))
	}
	if fs.NArg() > 0 {
		fatal("unknown flag: %s\nusage: rodney ax-tree [--depth N] [--json]", fs.Arg(0))
	}

	var depth *int
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "depth" {
			depth = depthVal
		}
	})

	_, _, page := withPage()
	result, err := proto.AccessibilityGetFullAXTree{Depth: depth}.Call(page)
	if err != nil {
		fatal("failed to get accessibility tree: %v", err)
	}

	if *jsonOutput {
		fmt.Println(formatAXTreeJSON(result.Nodes))
	} else {
		fmt.Print(formatAXTree(result.Nodes))
	}
}

func cmdAXFind(args []string) {
	fs := flag.NewFlagSet("ax-find", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "")
	role := fs.String("role", "", "")
	jsonOutput := fs.Bool("json", false, "")

	if err := fs.Parse(args); err != nil {
		fatal("unknown flag: %s\nusage: rodney ax-find [--name N] [--role R] [--json]", findUnknownFlag(args, fs))
	}
	if fs.NArg() > 0 {
		fatal("unknown flag: %s\nusage: rodney ax-find [--name N] [--role R] [--json]", fs.Arg(0))
	}

	_, _, page := withPage()
	nodes, err := queryAXNodes(page, *name, *role)
	if err != nil {
		fatal("query failed: %v", err)
	}

	if len(nodes) == 0 {
		fmt.Fprintln(os.Stderr, "No matching nodes")
		os.Exit(1)
	}

	if *jsonOutput {
		data, _ := json.MarshalIndent(nodes, "", "  ")
		fmt.Println(string(data))
	} else {
		fmt.Print(formatAXNodeList(nodes))
	}
}

func cmdAXNode(args []string) {
	// Pre-extract --json since it may appear after the positional selector
	jsonOutput := false
	var filtered []string
	for _, a := range args {
		if a == "--json" {
			jsonOutput = true
		} else {
			filtered = append(filtered, a)
		}
	}

	fs := flag.NewFlagSet("ax-node", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Parse(filtered)

	if fs.NArg() < 1 {
		fatal("usage: rodney ax-node <selector> [--json]")
	}
	selector := fs.Arg(0)

	_, _, page := withPage()
	node, err := getAXNode(page, selector)
	if err != nil {
		fatal("%v", err)
	}

	if jsonOutput {
		fmt.Println(formatAXNodeDetailJSON(node))
	} else {
		fmt.Print(formatAXNodeDetail(node))
	}
}

// queryAXNodes uses Accessibility.queryAXTree to find nodes by name and/or role.
func queryAXNodes(page *rod.Page, name, role string) ([]*proto.AccessibilityAXNode, error) {
	// Get the document node to use as query root
	zero := 0
	doc, err := proto.DOMGetDocument{Depth: &zero}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("failed to get document: %w", err)
	}

	result, err := proto.AccessibilityQueryAXTree{
		BackendNodeID:  doc.Root.BackendNodeID,
		AccessibleName: name,
		Role:           role,
	}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("accessibility query failed: %w", err)
	}

	return result.Nodes, nil
}

// getAXNode gets the accessibility node for a DOM element identified by CSS selector.
func getAXNode(page *rod.Page, selector string) (*proto.AccessibilityAXNode, error) {
	el, err := page.Element(selector)
	if err != nil {
		return nil, fmt.Errorf("element not found: %w", err)
	}

	// Describe the DOM node to get its backend node ID
	node, err := proto.DOMDescribeNode{ObjectID: el.Object.ObjectID}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("failed to describe DOM node: %w", err)
	}

	result, err := proto.AccessibilityGetPartialAXTree{
		BackendNodeID:  node.Node.BackendNodeID,
		FetchRelatives: false,
	}.Call(page)
	if err != nil {
		return nil, fmt.Errorf("failed to get accessibility info: %w", err)
	}

	// Find the non-ignored node (the first non-ignored node is typically our target)
	for _, n := range result.Nodes {
		if !n.Ignored {
			return n, nil
		}
	}

	// Fall back to first node if all are ignored
	if len(result.Nodes) > 0 {
		return result.Nodes[0], nil
	}

	return nil, fmt.Errorf("no accessibility node found for selector %q", selector)
}

// axValueStr extracts a printable string from an AccessibilityAXValue.
func axValueStr(v *proto.AccessibilityAXValue) string {
	if v == nil {
		return ""
	}
	raw := v.Value.JSON("", "")
	// Unquote JSON strings
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		var s string
		if err := json.Unmarshal([]byte(raw), &s); err == nil {
			return s
		}
	}
	return raw
}

// formatAXTree formats a flat list of AX nodes as an indented text tree.
// Ignored nodes are skipped.
func formatAXTree(nodes []*proto.AccessibilityAXNode) string {
	if len(nodes) == 0 {
		return ""
	}

	// Build lookup maps
	nodeByID := make(map[proto.AccessibilityAXNodeID]*proto.AccessibilityAXNode)
	for _, n := range nodes {
		nodeByID[n.NodeID] = n
	}

	// Find root (node with no parent or first node)
	var rootID proto.AccessibilityAXNodeID
	for _, n := range nodes {
		if n.ParentID == "" {
			rootID = n.NodeID
			break
		}
	}
	if rootID == "" && len(nodes) > 0 {
		rootID = nodes[0].NodeID
	}

	var sb strings.Builder
	var walk func(id proto.AccessibilityAXNodeID, depth int)
	walk = func(id proto.AccessibilityAXNodeID, depth int) {
		node, ok := nodeByID[id]
		if !ok {
			return
		}
		// Skip ignored nodes but still recurse into their children
		if !node.Ignored {
			indent := strings.Repeat("  ", depth)
			role := axValueStr(node.Role)
			name := axValueStr(node.Name)

			line := fmt.Sprintf("%s[%s]", indent, role)
			if name != "" {
				line += fmt.Sprintf(" %q", name)
			}

			// Append interesting properties
			props := formatProperties(node.Properties)
			if props != "" {
				line += " (" + props + ")"
			}

			sb.WriteString(line + "\n")
			// Children at depth+1
			for _, childID := range node.ChildIDs {
				walk(childID, depth+1)
			}
		} else {
			// Ignored node: pass through to children at same depth
			for _, childID := range node.ChildIDs {
				walk(childID, depth)
			}
		}
	}

	walk(rootID, 0)
	return sb.String()
}

// formatProperties formats the interesting AX properties into a comma-separated string.
func formatProperties(props []*proto.AccessibilityAXProperty) string {
	if len(props) == 0 {
		return ""
	}
	var parts []string
	for _, p := range props {
		val := axValueStr(p.Value)
		switch string(p.Name) {
		case "focusable", "disabled", "editable", "hidden", "required",
			"checked", "expanded", "selected", "modal", "multiline",
			"multiselectable", "readonly", "focused", "settable":
			// Boolean-ish properties: only show if true
			if val == "true" {
				parts = append(parts, string(p.Name))
			}
		case "level":
			parts = append(parts, fmt.Sprintf("level=%s", val))
		case "autocomplete", "hasPopup", "orientation", "live",
			"relevant", "valuemin", "valuemax", "valuetext",
			"roledescription", "keyshortcuts":
			if val != "" {
				parts = append(parts, fmt.Sprintf("%s=%s", p.Name, val))
			}
		}
	}
	return strings.Join(parts, ", ")
}

// formatAXTreeJSON formats nodes as a JSON array.
func formatAXTreeJSON(nodes []*proto.AccessibilityAXNode) string {
	data, err := json.MarshalIndent(nodes, "", "  ")
	if err != nil {
		return "[]"
	}
	return string(data)
}

// formatAXNodeList formats a list of nodes as single-line summaries.
func formatAXNodeList(nodes []*proto.AccessibilityAXNode) string {
	var sb strings.Builder
	for _, node := range nodes {
		role := axValueStr(node.Role)
		name := axValueStr(node.Name)
		line := fmt.Sprintf("[%s]", role)
		if name != "" {
			line += fmt.Sprintf(" %q", name)
		}
		if node.BackendDOMNodeID != 0 {
			line += fmt.Sprintf(" backendNodeId=%d", node.BackendDOMNodeID)
		}
		props := formatProperties(node.Properties)
		if props != "" {
			line += " (" + props + ")"
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

// formatAXNodeDetail formats a single node with all its properties in key: value format.
func formatAXNodeDetail(node *proto.AccessibilityAXNode) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("role: %s\n", axValueStr(node.Role)))
	if name := axValueStr(node.Name); name != "" {
		sb.WriteString(fmt.Sprintf("name: %s\n", name))
	}
	if desc := axValueStr(node.Description); desc != "" {
		sb.WriteString(fmt.Sprintf("description: %s\n", desc))
	}
	if val := axValueStr(node.Value); val != "" {
		sb.WriteString(fmt.Sprintf("value: %s\n", val))
	}
	for _, p := range node.Properties {
		val := axValueStr(p.Value)
		sb.WriteString(fmt.Sprintf("%s: %s\n", p.Name, val))
	}
	return sb.String()
}

// formatAXNodeDetailJSON formats a single node as JSON.
func formatAXNodeDetailJSON(node *proto.AccessibilityAXNode) string {
	data, err := json.MarshalIndent(node, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

// --- Emulation commands ---

// applyUserAgent sets the user agent override on the page via CDP.
func applyUserAgent(page *rod.Page, ua string) error {
	return (proto.NetworkSetUserAgentOverride{UserAgent: ua}).Call(page)
}

func cmdUA(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney ua <user-agent-string>")
	}
	ua := strings.Join(args, " ")
	_, _, page := withPage()
	if err := applyUserAgent(page, ua); err != nil {
		fatal("failed to set user agent: %v", err)
	}
	fmt.Printf("User agent set to: %s\n", ua)
}

// applyTimezone sets the timezone override on the page via CDP.
func applyTimezone(page *rod.Page, timezoneID string) error {
	return (proto.EmulationSetTimezoneOverride{TimezoneID: timezoneID}).Call(page)
}

func cmdTimezone(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney timezone <timezone-id>")
	}
	tz := args[0]
	_, _, page := withPage()
	if err := applyTimezone(page, tz); err != nil {
		fatal("failed to set timezone: %v", err)
	}
	fmt.Printf("Timezone set to: %s\n", tz)
}

// applyLocale sets the locale override on the page via CDP.
func applyLocale(page *rod.Page, locale string) error {
	return (proto.EmulationSetLocaleOverride{Locale: locale}).Call(page)
}

func cmdLocale(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney locale <locale>")
	}
	loc := args[0]
	_, _, page := withPage()
	if err := applyLocale(page, loc); err != nil {
		fatal("failed to set locale: %v", err)
	}
	fmt.Printf("Locale set to: %s\n", loc)
}

// applyGeolocation sets the geolocation override on the page via CDP.
func applyGeolocation(page *rod.Page, lat, lon float64) error {
	accuracy := 1.0
	return (proto.EmulationSetGeolocationOverride{
		Latitude:  &lat,
		Longitude: &lon,
		Accuracy:  &accuracy,
	}).Call(page)
}

func cmdGeo(args []string) {
	var lat, lon float64
	var hasLat, hasLon bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--lat":
			i++
			if i >= len(args) {
				fatal("missing value for --lat")
			}
			v, err := strconv.ParseFloat(args[i], 64)
			if err != nil {
				fatal("invalid latitude: %v", err)
			}
			lat = v
			hasLat = true
		case "--lon":
			i++
			if i >= len(args) {
				fatal("missing value for --lon")
			}
			v, err := strconv.ParseFloat(args[i], 64)
			if err != nil {
				fatal("invalid longitude: %v", err)
			}
			lon = v
			hasLon = true
		default:
			fatal("unknown flag: %s\nusage: rodney geo --lat <lat> --lon <lon>", args[i])
		}
	}

	if !hasLat || !hasLon {
		fatal("usage: rodney geo --lat <lat> --lon <lon>")
	}

	_, _, page := withPage()
	if err := applyGeolocation(page, lat, lon); err != nil {
		fatal("failed to set geolocation: %v", err)
	}
	fmt.Printf("Geolocation set to: lat=%f lon=%f\n", lat, lon)
}

// applyMedia sets media feature overrides on the page via CDP.
func applyMedia(page *rod.Page, features []*proto.EmulationMediaFeature) error {
	return (proto.EmulationSetEmulatedMedia{Features: features}).Call(page)
}

// applyMediaType sets the emulated media type (e.g. "print", "screen") on the page via CDP.
func applyMediaType(page *rod.Page, mediaType string) error {
	return (proto.EmulationSetEmulatedMedia{Media: mediaType}).Call(page)
}

func cmdMedia(args []string) {
	var mediaType string
	var features []*proto.EmulationMediaFeature

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--type":
			i++
			if i >= len(args) {
				fatal("missing value for --type")
			}
			mediaType = args[i]
		case "--feature":
			i++
			if i >= len(args) {
				fatal("missing value for --feature")
			}
			parts := strings.SplitN(args[i], "=", 2)
			if len(parts) != 2 {
				fatal("invalid feature format, expected name=value: %s", args[i])
			}
			features = append(features, &proto.EmulationMediaFeature{
				Name:  parts[0],
				Value: parts[1],
			})
		default:
			fatal("unknown flag: %s\nusage: rodney media [--type T] [--feature name=value ...]", args[i])
		}
	}

	if mediaType == "" && len(features) == 0 {
		fatal("usage: rodney media [--type T] [--feature name=value ...]")
	}

	_, _, page := withPage()
	req := proto.EmulationSetEmulatedMedia{
		Media:    mediaType,
		Features: features,
	}
	if err := req.Call(page); err != nil {
		fatal("failed to set media emulation: %v", err)
	}

	if mediaType != "" {
		fmt.Printf("Media type set to: %s\n", mediaType)
	}
	for _, f := range features {
		fmt.Printf("Media feature set: %s=%s\n", f.Name, f.Value)
	}
}

// --- Auth proxy for environments with authenticated HTTP proxies ---

// detectProxy checks for HTTPS_PROXY/HTTP_PROXY with credentials.
// Returns (proxyServer, username, password, true) if auth proxy is needed.
func detectProxy() (server, user, pass string, needed bool) {
	proxyEnv := os.Getenv("HTTPS_PROXY")
	if proxyEnv == "" {
		proxyEnv = os.Getenv("https_proxy")
	}
	if proxyEnv == "" {
		proxyEnv = os.Getenv("HTTP_PROXY")
	}
	if proxyEnv == "" {
		proxyEnv = os.Getenv("http_proxy")
	}
	if proxyEnv == "" {
		return "", "", "", false
	}
	parsed, err := url.Parse(proxyEnv)
	if err != nil || parsed.User == nil {
		return "", "", "", false
	}
	user = parsed.User.Username()
	pass, _ = parsed.User.Password()
	if user == "" {
		return "", "", "", false
	}
	server = parsed.Hostname() + ":" + parsed.Port()
	return server, user, pass, true
}

// --- Cookie commands ---

func parseCookieSetArgs(args []string) (*proto.NetworkCookieParam, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("usage: rodney cookie-set <name> <value> --domain <domain> [options]")
	}
	param := &proto.NetworkCookieParam{
		Name:  args[0],
		Value: args[1],
	}
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--domain":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--domain requires a value")
			}
			param.Domain = args[i]
		case "--url":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--url requires a value")
			}
			param.URL = args[i]
		case "--path":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--path requires a value")
			}
			param.Path = args[i]
		case "--secure":
			param.Secure = true
		case "--httponly":
			param.HTTPOnly = true
		case "--samesite":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--samesite requires a value")
			}
			switch args[i] {
			case "Strict":
				param.SameSite = proto.NetworkCookieSameSiteStrict
			case "Lax":
				param.SameSite = proto.NetworkCookieSameSiteLax
			case "None":
				param.SameSite = proto.NetworkCookieSameSiteNone
			default:
				return nil, fmt.Errorf("--samesite must be Strict, Lax, or None")
			}
		case "--expires":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--expires requires a value")
			}
			ts, err := strconv.ParseFloat(args[i], 64)
			if err != nil {
				return nil, fmt.Errorf("invalid --expires value: %v", err)
			}
			param.Expires = proto.TimeSinceEpoch(ts)
		default:
			return nil, fmt.Errorf("unknown flag: %s", args[i])
		}
	}
	return param, nil
}

func setCookieOnBrowser(page *rod.Page, param *proto.NetworkCookieParam) {
	err := proto.NetworkSetCookies{
		Cookies: []*proto.NetworkCookieParam{param},
	}.Call(page)
	if err != nil {
		fatal("failed to set cookie: %v", err)
	}
}

func cmdCookieSet(args []string) {
	param, err := parseCookieSetArgs(args)
	if err != nil {
		fatal("%v", err)
	}
	_, _, page := withPage()
	// Default to current page URL (like document.cookie= in JS)
	if param.Domain == "" && param.URL == "" {
		info, err := page.Info()
		if err != nil {
			fatal("failed to get page info: %v", err)
		}
		param.URL = info.URL
	}
	setCookieOnBrowser(page, param)
}

func parseCookieGetArgs(args []string) (name string, urls []string, jsonOutput bool) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--domain":
			i++
			if i < len(args) {
				urls = append(urls, "https://"+args[i]+"/")
			}
		case "--url":
			i++
			if i < len(args) {
				urls = append(urls, args[i])
			}
		case "--json":
			jsonOutput = true
		default:
			if name == "" {
				name = args[i]
			}
		}
	}
	return
}

func getCookiesFromBrowser(page *rod.Page, urls []string) []*proto.NetworkCookie {
	result, err := proto.NetworkGetCookies{Urls: urls}.Call(page)
	if err != nil {
		fatal("failed to get cookies: %v", err)
	}
	return result.Cookies
}

func formatCookiesDefault(cookies []*proto.NetworkCookie) string {
	var lines []string
	for _, c := range cookies {
		parts := []string{
			"name=" + c.Name,
			"value=" + c.Value,
			"domain=" + c.Domain,
			"path=" + c.Path,
		}
		if c.Secure {
			parts = append(parts, "secure")
		}
		if c.HTTPOnly {
			parts = append(parts, "httponly")
		}
		if c.Session {
			parts = append(parts, "session")
		} else {
			t := time.Unix(int64(c.Expires), 0).UTC()
			parts = append(parts, "expires="+t.Format(time.RFC3339))
		}
		lines = append(lines, strings.Join(parts, "\t"))
	}
	return strings.Join(lines, "\n")
}

func formatCookiesJSON(cookies []*proto.NetworkCookie) string {
	data, err := json.MarshalIndent(cookies, "", "  ")
	if err != nil {
		fatal("failed to marshal cookies: %v", err)
	}
	return string(data)
}

func cmdCookieGet(args []string) {
	name, urls, jsonOutput := parseCookieGetArgs(args)
	_, _, page := withPage()
	cookies := getCookiesFromBrowser(page, urls)

	if name != "" && !jsonOutput {
		// Print just the value of the first matching cookie
		for _, c := range cookies {
			if c.Name == name {
				fmt.Println(c.Value)
				return
			}
		}
		return
	}

	if name != "" {
		// Filter to matching name for JSON output
		var filtered []*proto.NetworkCookie
		for _, c := range cookies {
			if c.Name == name {
				filtered = append(filtered, c)
			}
		}
		cookies = filtered
	}

	if jsonOutput {
		fmt.Println(formatCookiesJSON(cookies))
	} else {
		out := formatCookiesDefault(cookies)
		if out != "" {
			fmt.Println(out)
		}
	}
}

func parseCookieDeleteArgs(args []string) (name, domain, cookieURL, path string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--domain":
			i++
			if i >= len(args) {
				return "", "", "", "", fmt.Errorf("--domain requires a value")
			}
			domain = args[i]
		case "--url":
			i++
			if i >= len(args) {
				return "", "", "", "", fmt.Errorf("--url requires a value")
			}
			cookieURL = args[i]
		case "--path":
			i++
			if i >= len(args) {
				return "", "", "", "", fmt.Errorf("--path requires a value")
			}
			path = args[i]
		default:
			if name == "" {
				name = args[i]
			} else {
				return "", "", "", "", fmt.Errorf("unknown flag: %s", args[i])
			}
		}
	}
	if name == "" {
		return "", "", "", "", fmt.Errorf("usage: rodney cookie-delete <name> [--domain <domain>] [--url <url>] [--path <path>]")
	}
	return
}

func deleteCookieFromBrowser(page *rod.Page, name, domain, cookieURL, path string) {
	if domain != "" || cookieURL != "" || path != "" {
		// Direct delete with specified filters
		err := proto.NetworkDeleteCookies{
			Name:   name,
			Domain: domain,
			URL:    cookieURL,
			Path:   path,
		}.Call(page)
		if err != nil {
			fatal("failed to delete cookie: %v", err)
		}
		return
	}
	// No filters: find all cookies with this name and delete each
	cookies := getCookiesFromBrowser(page, nil)
	for _, c := range cookies {
		if c.Name == name {
			err := proto.NetworkDeleteCookies{
				Name:   name,
				Domain: c.Domain,
				Path:   c.Path,
			}.Call(page)
			if err != nil {
				fatal("failed to delete cookie %s (domain=%s): %v", name, c.Domain, err)
			}
		}
	}
}

func cmdCookieDelete(args []string) {
	name, domain, cookieURL, path, err := parseCookieDeleteArgs(args)
	if err != nil {
		fatal("%v", err)
	}
	_, _, page := withPage()
	deleteCookieFromBrowser(page, name, domain, cookieURL, path)
}

func clearCookiesForDomain(page *rod.Page, domain string) {
	cookies := getCookiesFromBrowser(page, []string{"https://" + domain + "/"})
	for _, c := range cookies {
		err := proto.NetworkDeleteCookies{
			Name:   c.Name,
			Domain: c.Domain,
			Path:   c.Path,
		}.Call(page)
		if err != nil {
			fatal("failed to delete cookie %s: %v", c.Name, err)
		}
	}
}

func cmdCookieClear(args []string) {
	var domain string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--domain":
			i++
			if i >= len(args) {
				fatal("--domain requires a value")
			}
			domain = args[i]
		default:
			fatal("unknown flag: %s\nusage: rodney cookie-clear [--domain <domain>]", args[i])
		}
	}
	_, _, page := withPage()
	if domain != "" {
		clearCookiesForDomain(page, domain)
		fmt.Printf("Cookies cleared for %s\n", domain)
	} else {
		err := proto.NetworkClearBrowserCookies{}.Call(page)
		if err != nil {
			fatal("failed to clear cookies: %v", err)
		}
		fmt.Println("All cookies cleared")
	}
}

// consoleEntry is one line in the console.jsonl buffer.
type consoleEntry struct {
	Source    string   `json:"source"`          // "console" or "browser"
	Type      string   `json:"type,omitempty"`  // console method: log, warn, error, ...
	Level     string   `json:"level,omitempty"` // browser log level: verbose, info, warning, error
	Timestamp float64  `json:"timestamp"`       // ms since epoch
	Args      []string `json:"args,omitempty"`  // serialized console args
	Text      string   `json:"text,omitempty"`  // browser log message
	URL       string   `json:"url,omitempty"`
	Line      int      `json:"line,omitempty"`
	Column    int      `json:"column,omitempty"`
}

// parseConsoleLevel maps a user-supplied --level value to the set of matching
// console methods / browser levels. Unknown levels are an error.
func parseConsoleLevel(level string) (map[string]bool, map[string]bool, error) {
	consoleTypes := map[string]bool{}
	browserLevels := map[string]bool{}
	switch level {
	case "":
		return nil, nil, nil
	case "log":
		consoleTypes["log"] = true
	case "info":
		consoleTypes["info"] = true
		browserLevels["info"] = true
	case "warn", "warning":
		consoleTypes["warning"] = true
		browserLevels["warning"] = true
	case "error":
		consoleTypes["error"] = true
		browserLevels["error"] = true
	case "debug":
		consoleTypes["debug"] = true
		browserLevels["verbose"] = true
	default:
		return nil, nil, fmt.Errorf("unknown level %q (use log, info, warn, error or debug)", level)
	}
	return consoleTypes, browserLevels, nil
}

// matchesConsoleEntry reports whether an entry passes the --level/--browser filters.
func matchesConsoleEntry(e consoleEntry, level string, includeBrowser bool) bool {
	if e.Source == "browser" {
		if !includeBrowser {
			return false
		}
		if level == "" {
			return true
		}
		_, browserLevels, _ := parseConsoleLevel(level)
		return browserLevels[e.Level]
	}
	if level == "" {
		return true
	}
	consoleTypes, _, _ := parseConsoleLevel(level)
	return consoleTypes[e.Type]
}

// formatConsoleEntry renders one entry in human-readable or JSON form.
func formatConsoleEntry(e consoleEntry, asJSON bool) string {
	if asJSON {
		b, err := json.Marshal(e)
		if err != nil {
			return fmt.Sprintf("{\"error\":\"marshal failed\"}")
		}
		return string(b)
	}
	if e.Source == "browser" {
		return fmt.Sprintf("[browser/%s] %s", e.Level, e.Text)
	}
	msg := strings.Join(e.Args, " ")
	return fmt.Sprintf("[%s] %s", e.Type, msg)
}

// consoleArgsToStrings converts RuntimeRemoteObject args to display strings.
func consoleArgsToStrings(page *rod.Page, remoteArgs []*proto.RuntimeRemoteObject) []string {
	args := []string{}
	for _, a := range remoteArgs {
		if v, err := page.ObjectToJSON(a); err == nil {
			switch val := v.Val().(type) {
			case string:
				args = append(args, val)
			default:
				b, err := json.Marshal(val)
				if err != nil {
					args = append(args, fmt.Sprintf("%v", val))
				} else {
					args = append(args, string(b))
				}
			}
		} else {
			args = append(args, fmt.Sprintf("%v", a))
		}
	}
	return args
}

// consoleEntryFromConsoleEvent builds a consoleEntry from a Runtime.consoleAPICalled event.
func consoleEntryFromConsoleEvent(e *proto.RuntimeConsoleAPICalled) consoleEntry {
	entry := consoleEntry{
		Source:    "console",
		Type:      string(e.Type),
		Timestamp: float64(e.Timestamp),
	}
	if e.StackTrace != nil && len(e.StackTrace.CallFrames) > 0 {
		entry.URL = e.StackTrace.CallFrames[0].URL
		entry.Line = e.StackTrace.CallFrames[0].LineNumber
		entry.Column = e.StackTrace.CallFrames[0].ColumnNumber
	}
	return entry
}

// consoleEntryFromBrowserEvent builds a consoleEntry from a Log.entryAdded event.
func consoleEntryFromBrowserEvent(e *proto.LogEntryAdded) consoleEntry {
	entry := consoleEntry{
		Source:    "browser",
		Level:     string(e.Entry.Level),
		Timestamp: float64(e.Entry.Timestamp),
		Text:      e.Entry.Text,
		URL:       e.Entry.URL,
	}
	if e.Entry.LineNumber != nil {
		entry.Line = *e.Entry.LineNumber
	}
	return entry
}

// streamConsoleEvents subscribes to console events on the given page and prints
// them (filtered) until the process is interrupted. Blocking.
func streamConsoleEvents(page *rod.Page, level string, asJSON, includeBrowser bool) {
	// Buffer output so prints from the event goroutine don't interleave.
	var mu sync.Mutex
	printEntry := func(e consoleEntry) {
		if !matchesConsoleEntry(e, level, includeBrowser) {
			return
		}
		mu.Lock()
		fmt.Println(formatConsoleEntry(e, asJSON))
		mu.Unlock()
	}

	// EachEvent blocks until the returned wait func returns; we run until SIGINT.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()

	// EachEvent returns a wait func that consumes the event channel and fires
	// the callbacks — it must RUN. We call it in a goroutine and block in select{}
	// until SIGINT (handled above exits the process).
	wait := page.EachEvent(
		func(e *proto.RuntimeConsoleAPICalled) {
			entry := consoleEntryFromConsoleEvent(e)
			entry.Args = consoleArgsToStrings(page, e.Args)
			printEntry(entry)
		},
		func(e *proto.LogEntryAdded) {
			printEntry(consoleEntryFromBrowserEvent(e))
		},
	)
	go wait()
	select {}
}
func readConsoleBuffer(path, level string, asJSON, includeBrowser bool) int {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		fatal("failed to open console buffer: %v", err)
	}
	defer f.Close()

	n := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var e consoleEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // skip malformed lines
		}
		if !matchesConsoleEntry(e, level, includeBrowser) {
			continue
		}
		fmt.Println(formatConsoleEntry(e, asJSON))
		n++
	}
	return n
}

// collectorRunning reports whether a console collector with the given PID is alive.
func collectorRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func cmdConsole(args []string) {
	var level string
	var asJSON, includeBrowser, follow, clear bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--level":
			i++
			if i >= len(args) {
				fatal("--level requires a value")
			}
			level = args[i]
			if _, _, err := parseConsoleLevel(level); err != nil {
				fatal("%v", err)
			}
		case "--json":
			asJSON = true
		case "--browser":
			includeBrowser = true
		case "--follow":
			follow = true
		case "--clear":
			clear = true
		default:
			fatal("unknown flag: %s\nusage: rodney console [--level L] [--json] [--browser] [--follow] [--clear]", args[i])
		}
	}

	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}

	// Phase 2: collector running -> read buffer (unless --follow streams live too)
	if collectorRunning(s.ConsolePID) && s.ConsoleLog != "" {
		n := readConsoleBuffer(s.ConsoleLog, level, asJSON, includeBrowser)
		if clear {
			os.Truncate(s.ConsoleLog, 0)
		}
		if !follow {
			if n == 0 {
				fmt.Println("(no matching console messages)")
			}
			return
		}
		// --follow: print buffered, then tail for new lines
		tailConsoleBuffer(s.ConsoleLog, level, asJSON, includeBrowser)
		return
	}

	// Phase 1: no collector -> live streaming on the active page
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	page, err := getActivePage(browser, s)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Fprintln(os.Stderr, "(streaming console — Ctrl+C to stop; use console-start for a background buffer)")
	streamConsoleEvents(page, level, asJSON, includeBrowser)
}

// tailConsoleBuffer follows the JSONL file like tail -f and prints matching new entries.
func tailConsoleBuffer(path, level string, asJSON, includeBrowser bool) {
	f, err := os.Open(path)
	if err != nil {
		fatal("failed to open console buffer: %v", err)
	}
	defer f.Close()
	// Start at end: buffered entries were already printed
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		fatal("seek failed: %v", err)
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// No new data yet — poll
			if err == io.EOF {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			fatal("read failed: %v", err)
		}
		if len(strings.TrimSpace(line)) == 0 {
			continue
		}
		var e consoleEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if matchesConsoleEntry(e, level, includeBrowser) {
			fmt.Println(formatConsoleEntry(e, asJSON))
		}
	}
}

func cmdConsoleStart(args []string) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	if collectorRunning(s.ConsolePID) {
		fmt.Println("Console collector already running")
		return
	}
	// Verify the browser is reachable before spawning
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	page, err := getActivePage(browser, s)
	if err != nil {
		fatal("%v", err)
	}
	targetID := string(page.TargetID)

	logPath := filepath.Join(stateDir(), "console.jsonl")
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "_console", s.DebugURL, targetID, logPath)
	setSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		fatal("failed to start console collector: %v", err)
	}
	// Read the PID BEFORE Release() — Release invalidates cmd.Process on some platforms.
	pid := cmd.Process.Pid
	cmd.Process.Release()

	s.ConsolePID = pid
	s.ConsoleLog = logPath
	if err := saveState(s); err != nil {
		fatal("failed to save state: %v", err)
	}
	fmt.Printf("Console collector started (PID %d) -> %s\n", s.ConsolePID, logPath)
}

func cmdConsoleStop(args []string) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	if !collectorRunning(s.ConsolePID) {
		// Clean up stale state
		s.ConsolePID = 0
		s.ConsoleLog = ""
		saveState(s)
		fmt.Println("No console collector running")
		return
	}
	if proc, err := os.FindProcess(s.ConsolePID); err == nil {
		proc.Signal(syscall.SIGTERM)
	}
	s.ConsolePID = 0
	logPath := s.ConsoleLog
	s.ConsoleLog = ""
	saveState(s)
	if logPath != "" {
		os.Remove(logPath)
	}
	fmt.Println("Console collector stopped")
}

// cmdInternalConsole is a hidden subcommand: rodney _console <debug-url> <targetID> <log-path>
// It stays connected to Chrome and appends console events to the JSONL buffer.
func cmdInternalConsole(args []string) {
	if len(args) != 3 {
		fatal("usage: rodney _console <debug-url> <targetID> <log-path>")
	}
	debugURL, targetID, logPath := args[0], args[1], args[2]

	browser := rod.New().ControlURL(debugURL)
	if err := browser.Connect(); err != nil {
		fatal("_console: connect failed: %v", err)
	}
	defer browser.Close()

	page, err := browser.PageFromTarget(proto.TargetTargetID(targetID))
	if err != nil {
		fatal("_console: page not found: %v", err)
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal("_console: open log failed: %v", err)
	}
	defer f.Close()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()

	wait := page.EachEvent(
		func(e *proto.RuntimeConsoleAPICalled) {
			entry := consoleEntryFromConsoleEvent(e)
			entry.Args = consoleArgsToStrings(page, e.Args)
			writeConsoleLine(f, entry)
		},
		func(e *proto.LogEntryAdded) {
			writeConsoleLine(f, consoleEntryFromBrowserEvent(e))
		},
	)
	go wait()
	select {}
}

// writeConsoleLine serializes one entry and appends it to the JSONL file.
var consoleWriteMu sync.Mutex

func writeConsoleLine(f *os.File, e consoleEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	consoleWriteMu.Lock()
	defer consoleWriteMu.Unlock()
	f.Write(append(b, '\n'))
}

// writeRawLine appends one pre-marshaled JSON line to a log file (thread-safe).
func writeRawLine(f *os.File, b []byte) {
	consoleWriteMu.Lock()
	defer consoleWriteMu.Unlock()
	f.Write(append(b, '\n'))
}

// cmdInternalProxy is a hidden subcommand: rodney _proxy <port> <upstream> <authHeader>
// It runs a local auth proxy that forwards to the upstream proxy with credentials.
func cmdInternalProxy(args []string) {
	if len(args) < 3 {
		fatal("usage: rodney _proxy <port> <upstream> <authHeader>")
	}
	port := args[0]
	upstream := args[1]
	authHeader := args[2]

	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fatal("proxy listen failed: %v", err)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				proxyConnect(w, r, upstream, authHeader)
			} else {
				proxyHTTP(w, r, upstream, authHeader)
			}
		}),
	}
	server.Serve(listener) // blocks forever
}

func proxyConnect(w http.ResponseWriter, r *http.Request, upstream, authHeader string) {
	upstreamConn, err := net.DialTimeout("tcp", upstream, 30*time.Second)
	if err != nil {
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		r.Host, r.Host, authHeader)
	if _, err := upstreamConn.Write([]byte(connectReq)); err != nil {
		upstreamConn.Close()
		http.Error(w, "upstream write failed", http.StatusBadGateway)
		return
	}

	buf := make([]byte, 4096)
	n, err := upstreamConn.Read(buf)
	if err != nil {
		upstreamConn.Close()
		http.Error(w, "upstream read failed", http.StatusBadGateway)
		return
	}
	response := string(buf[:n])
	if len(response) < 12 || response[9:12] != "200" {
		upstreamConn.Close()
		http.Error(w, "upstream rejected CONNECT", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstreamConn.Close()
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		upstreamConn.Close()
		return
	}

	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	go func() {
		io.Copy(upstreamConn, clientConn)
		upstreamConn.Close()
	}()
	go func() {
		io.Copy(clientConn, upstreamConn)
		clientConn.Close()
	}()
}

func proxyHTTP(w http.ResponseWriter, r *http.Request, upstream, authHeader string) {
	proxyURL, _ := url.Parse("http://" + upstream)
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		ProxyConnectHeader: http.Header{
			"Proxy-Authorization": {authHeader},
		},
	}
	r.Header.Set("Proxy-Authorization", authHeader)

	resp, err := transport.RoundTrip(r)
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// cmdDialog handles JavaScript dialogs (alert/confirm/prompt/beforeunload).
// Runs as a persistent FOREGROUND process (like mock/block): arms a handler
// on the active page BEFORE dialogs open — an already-open dialog cannot be
// reached from a second connection (CDP blocks while a modal is showing),
// so the handler must be subscribed first. Handles every dialog until Ctrl+C.
func cmdDialog(args []string) {
	var dismiss, asJSON bool
	var text string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dismiss":
			dismiss = true
		case "--json":
			asJSON = true
		case "--text":
			i++
			if i >= len(args) {
				fatal("--text requires a value")
			}
			text = args[i]
		default:
			fatal("unknown flag: %s\nusage: rodney dialog [--dismiss] [--text MSG] [--json]", args[i])
		}
	}
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	page, err := getActivePage(browser, s)
	if err != nil {
		fatal("%v", err)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()

	action := "accepted"
	if dismiss {
		action = "dismissed"
	}
	fmt.Fprintf(os.Stderr, "(handling dialogs on the active page — %s — Ctrl+C to stop)\n", action)

	for {
		wait, handle := page.HandleDialog()
		e := wait()
		if asJSON {
			b, _ := json.Marshal(map[string]string{
				"type": string(e.Type), "message": e.Message, "default_prompt": e.DefaultPrompt, "action": action,
			})
			fmt.Println(string(b))
		} else {
			fmt.Printf("[%s] %s — %s\n", e.Type, e.Message, action)
		}
		if err := handle(&proto.PageHandleJavaScriptDialog{Accept: !dismiss, PromptText: text}); err != nil {
			fmt.Fprintf(os.Stderr, "failed to handle dialog: %v\n", err)
		}
	}
}

// requestEntry is one line in the requests.jsonl buffer (one CDP event each).
type requestEntry struct {
	Event     string  `json:"event"` // request | response | failed
	RequestID string  `json:"request_id"`
	Method    string  `json:"method,omitempty"`
	URL       string  `json:"url,omitempty"`
	Status    int     `json:"status,omitempty"`
	Resource  string  `json:"resource,omitempty"`
	Error     string  `json:"error,omitempty"`
	Timestamp float64 `json:"timestamp"`
}

// requestPair is a rendered request line (request + its response/failed paired).
type requestPair struct {
	Method    string  `json:"method"`
	URL       string  `json:"url"`
	Status    int     `json:"status"`
	Error     string  `json:"error,omitempty"`
	Resource  string  `json:"resource,omitempty"`
	Timestamp float64 `json:"timestamp"`
}

// pairRequestEntries pairs raw events by request_id into request pairs.
func pairRequestEntries(entries []requestEntry) []requestPair {
	reqs := map[string]requestEntry{}
	var order []string
	var out []requestPair
	for _, e := range entries {
		switch e.Event {
		case "request":
			reqs[e.RequestID] = e
			order = append(order, e.RequestID)
		case "response", "failed":
			r, ok := reqs[e.RequestID]
			if !ok {
				continue
			}
			delete(reqs, e.RequestID)
			out = append(out, requestPair{
				Method: r.Method, URL: r.URL, Status: e.Status,
				Error: e.Error, Resource: r.Resource, Timestamp: e.Timestamp,
			})
		}
	}
	// In-flight requests (no response yet)
	for _, id := range order {
		if r, ok := reqs[id]; ok {
			out = append(out, requestPair{Method: r.Method, URL: r.URL, Resource: r.Resource, Timestamp: r.Timestamp})
		}
	}
	return out
}

// formatRequestPair renders one pair for humans.
func formatRequestPair(p requestPair) string {
	status := "..."
	if p.Status > 0 {
		status = strconv.Itoa(p.Status)
	}
	if p.Error != "" {
		status = "ERR"
	}
	return fmt.Sprintf("%s %s -> %s", p.Method, p.URL, status)
}

// streamRequestEvents subscribes to network events on the page and prints
// paired request lines until interrupted. Blocking.
func streamRequestEvents(page *rod.Page, asJSON bool) {
	var mu sync.Mutex
	var pending map[string]requestEntry = map[string]requestEntry{}
	printLine := func(line string) {
		mu.Lock()
		fmt.Println(line)
		mu.Unlock()
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()
	wait := page.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			mu.Lock()
			pending[string(e.RequestID)] = requestEntry{Event: "request", RequestID: string(e.RequestID), Method: e.Request.Method, URL: e.Request.URL, Resource: string(e.Type), Timestamp: float64(e.Timestamp)}
			mu.Unlock()
			if asJSON {
				b, _ := json.Marshal(requestEntry{Event: "request", RequestID: string(e.RequestID), Method: e.Request.Method, URL: e.Request.URL, Resource: string(e.Type), Timestamp: float64(e.Timestamp)})
				printLine(string(b))
			} else {
				printLine(fmt.Sprintf("%s %s -> ...", e.Request.Method, e.Request.URL))
			}
		},
		func(e *proto.NetworkResponseReceived) {
			if asJSON {
				b, _ := json.Marshal(requestEntry{Event: "response", RequestID: string(e.RequestID), Status: e.Response.Status, Timestamp: float64(e.Timestamp)})
				printLine(string(b))
				return
			}
			mu.Lock()
			r, ok := pending[string(e.RequestID)]
			delete(pending, string(e.RequestID))
			mu.Unlock()
			if !ok {
				return
			}
			printLine(fmt.Sprintf("%s %s -> %d", r.Method, r.URL, e.Response.Status))
		},
		func(e *proto.NetworkLoadingFailed) {
			if asJSON {
				b, _ := json.Marshal(requestEntry{Event: "failed", RequestID: string(e.RequestID), Error: e.ErrorText, Timestamp: float64(e.Timestamp)})
				printLine(string(b))
				return
			}
			mu.Lock()
			r, ok := pending[string(e.RequestID)]
			delete(pending, string(e.RequestID))
			mu.Unlock()
			if !ok {
				return
			}
			printLine(fmt.Sprintf("%s %s -> ERR %s", r.Method, r.URL, e.ErrorText))
		},
	)
	go wait()
	select {}
}

// readRequestBuffer prints paired entries from the requests.jsonl file.
func readRequestBuffer(path string, asJSON bool) int {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		fatal("failed to open request buffer: %v", err)
	}
	defer f.Close()
	var entries []requestEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var e requestEntry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	n := 0
	for _, p := range pairRequestEntries(entries) {
		if asJSON {
			b, _ := json.Marshal(p)
			fmt.Println(string(b))
		} else {
			fmt.Println(formatRequestPair(p))
		}
		n++
	}
	return n
}

func cmdRequests(args []string) {
	var asJSON, follow, clear bool
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "--follow":
			follow = true
		case "--clear":
			clear = true
		default:
			fatal("unknown flag: %s\nusage: rodney requests [--json] [--follow] [--clear]", a)
		}
	}
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	if collectorRunning(s.RequestPID) && s.RequestLog != "" {
		n := readRequestBuffer(s.RequestLog, asJSON)
		if clear {
			os.Truncate(s.RequestLog, 0)
		}
		if !follow {
			if n == 0 {
				fmt.Println("(no requests captured)")
			}
			return
		}
		tailRequestBuffer(s.RequestLog, asJSON)
		return
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	page, err := getActivePage(browser, s)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Fprintln(os.Stderr, "(streaming requests — Ctrl+C to stop; use requests-start for a background buffer)")
	streamRequestEvents(page, asJSON)
}

// tailRequestBuffer follows the JSONL file and prints paired new entries.
func tailRequestBuffer(path string, asJSON bool) {
	f, err := os.Open(path)
	if err != nil {
		fatal("failed to open request buffer: %v", err)
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		fatal("seek failed: %v", err)
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()
	reader := bufio.NewReader(f)
	var pending map[string]requestEntry = map[string]requestEntry{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			fatal("read failed: %v", err)
		}
		if len(strings.TrimSpace(line)) == 0 {
			continue
		}
		var e requestEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		switch e.Event {
		case "request":
			pending[e.RequestID] = e
			if !asJSON {
				fmt.Printf("%s %s -> ...\n", e.Method, e.URL)
			} else {
				fmt.Println(line)
			}
		case "response", "failed":
			if r, ok := pending[e.RequestID]; ok {
				delete(pending, e.RequestID)
				if asJSON {
					fmt.Println(line)
				} else if e.Event == "response" {
					fmt.Printf("%s %s -> %d\n", r.Method, r.URL, e.Status)
				} else {
					fmt.Printf("%s %s -> ERR %s\n", r.Method, r.URL, e.Error)
				}
			} else if asJSON {
				fmt.Println(line)
			}
		}
	}
}

func cmdRequestsStart(args []string) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	if collectorRunning(s.RequestPID) {
		fmt.Println("Request collector already running")
		return
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	page, err := getActivePage(browser, s)
	if err != nil {
		fatal("%v", err)
	}
	targetID := string(page.TargetID)

	logPath := filepath.Join(stateDir(), "requests.jsonl")
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "_requests", s.DebugURL, targetID, logPath)
	setSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		fatal("failed to start request collector: %v", err)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()

	s.RequestPID = pid
	s.RequestLog = logPath
	if err := saveState(s); err != nil {
		fatal("failed to save state: %v", err)
	}
	fmt.Printf("Request collector started (PID %d) -> %s\n", pid, logPath)
}

func cmdRequestsStop(args []string) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	if !collectorRunning(s.RequestPID) {
		s.RequestPID = 0
		s.RequestLog = ""
		saveState(s)
		fmt.Println("No request collector running")
		return
	}
	if proc, err := os.FindProcess(s.RequestPID); err == nil {
		proc.Signal(syscall.SIGTERM)
	}
	s.RequestPID = 0
	logPath := s.RequestLog
	s.RequestLog = ""
	saveState(s)
	if logPath != "" {
		os.Remove(logPath)
	}
	fmt.Println("Request collector stopped")
}

// cmdInternalRequests is a hidden subcommand: rodney _requests <debug-url> <targetID> <log-path>
func cmdInternalRequests(args []string) {
	if len(args) != 3 {
		fatal("usage: rodney _requests <debug-url> <targetID> <log-path>")
	}
	debugURL, targetID, logPath := args[0], args[1], args[2]

	browser := rod.New().ControlURL(debugURL)
	if err := browser.Connect(); err != nil {
		fatal("_requests: connect failed: %%v", err)
	}
	defer browser.Close()

	page, err := browser.PageFromTarget(proto.TargetTargetID(targetID))
	if err != nil {
		fatal("_requests: page not found: %%v", err)
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal("_requests: open log failed: %%v", err)
	}
	defer f.Close()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()

	wait := page.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			writeRawLine(f, mustMarshal(requestEntry{Event: "request", RequestID: string(e.RequestID), Method: e.Request.Method, URL: e.Request.URL, Resource: string(e.Type), Timestamp: float64(e.Timestamp)}))
		},
		func(e *proto.NetworkResponseReceived) {
			writeRawLine(f, mustMarshal(requestEntry{Event: "response", RequestID: string(e.RequestID), Status: e.Response.Status, Timestamp: float64(e.Timestamp)}))
		},
		func(e *proto.NetworkLoadingFailed) {
			writeRawLine(f, mustMarshal(requestEntry{Event: "failed", RequestID: string(e.RequestID), Error: e.ErrorText, Timestamp: float64(e.Timestamp)}))
		},
	)
	go wait()
	select {}
}

// mustMarshal is a nil-safe JSON marshal helper for log lines.
func mustMarshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"event":"error"}`)
	}
	return b
}

// devicePresets maps friendly names to rod device presets.
var devicePresets = map[string]devices.Device{
	"iphone-se":     devices.IPhone4,
	"iphone-6":      devices.IPhone6or7or8,
	"iphone-6-plus": devices.IPhone6or7or8Plus,
	"iphone-x":      devices.IPhoneX,
	"ipad":          devices.IPad,
	"pixel-2":       devices.Pixel2,
	"pixel-2-xl":    devices.Pixel2XL,
	"nexus-5":       devices.Nexus5,
	"nexus-6":       devices.Nexus6,
	"galaxy-s3":     devices.GalaxySIII,
	"galaxy-s5":     devices.GalaxyS5,
	"laptop":        devices.LaptopWithMDPIScreen,
}

// cmdViewport sets the page viewport (CDP override persists on the target
// across CLI invocations until cleared or the browser stops).
func cmdViewport(args []string) {
	clear := false
	var rest []string
	for _, a := range args {
		if a == "--clear" {
			clear = true
		} else {
			rest = append(rest, a)
		}
	}
	if clear {
		s, err := loadState()
		if err != nil {
			fatal("%v", err)
		}
		s.ViewportW, s.ViewportH = 0, 0
		if err := saveState(s); err != nil {
			fatal("failed to save state: %v", err)
		}
		fmt.Println("Viewport cleared")
		return
	}
	if len(rest) != 2 {
		fatal("usage: rodney viewport <width> <height> [--clear]")
	}
	w, err := strconv.Atoi(rest[0])
	if err != nil || w < 1 {
		fatal("invalid width: %s", rest[0])
	}
	h, err := strconv.Atoi(rest[1])
	if err != nil || h < 1 {
		fatal("invalid height: %s", rest[1])
	}
	s, _, page := withPage()
	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: w, Height: h, DeviceScaleFactor: 1,
	}); err != nil {
		fatal("failed to set viewport: %v", err)
	}
	s.ViewportW, s.ViewportH = w, h
	if err := saveState(s); err != nil {
		fatal("failed to save state: %v", err)
	}
	fmt.Printf("Viewport set to %dx%d (persisted)\n", w, h)
}

// cmdDevice emulates a device preset: viewport + DPR + touch + user agent.
func cmdDevice(args []string) {
	var landscape, clear, list bool
	var name string
	for _, a := range args {
		switch a {
		case "--landscape":
			landscape = true
		case "--clear":
			clear = true
		case "--list":
			list = true
		default:
			if name != "" {
				fatal("unexpected argument: %s", a)
			}
			name = strings.ToLower(a)
		}
	}
	if list {
		keys := make([]string, 0, len(devicePresets))
		for k := range devicePresets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			d := devicePresets[k]
			fmt.Printf("%-15s %s (portrait %dx%d)\n", k, d.Title, d.Screen.Vertical.Width, d.Screen.Vertical.Height)
		}
		return
	}
	if clear {
		s, err := loadState()
		if err != nil {
			fatal("%v", err)
		}
		s.DeviceName = ""
		s.DeviceLandscape = false
		s.ViewportW, s.ViewportH = 0, 0
		if err := saveState(s); err != nil {
			fatal("failed to save state: %v", err)
		}
		fmt.Println("Device emulation cleared")
		return
	}
	if name == "" {
		fatal("usage: rodney device <name> [--landscape] [--clear] [--list]")
	}
	dev, ok := devicePresets[name]
	if !ok {
		fatal("unknown device %q (list with: rodney device --list)", name)
	}
	s, _, page := withPage()
	if landscape {
		dev = dev.Landscape()
	}
	if err := page.Emulate(dev); err != nil {
		fatal("failed to emulate device: %v", err)
	}
	s.DeviceName = name
	s.DeviceLandscape = landscape
	s.ViewportW, s.ViewportH = 0, 0 // device preset includes its own viewport
	if err := saveState(s); err != nil {
		fatal("failed to save state: %v", err)
	}
	fmt.Printf("Emulating %s (persisted)\n", dev.Title)
}

// cmdWaitNav waits until the page's URL changes from its current value.
func cmdWaitNav(args []string) {
	_, _, page := withPage()
	url, err := waitURLChanges(page, defaultTimeout)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Println(url)
}

// waitURLContains polls location.href until it contains substr.
func waitURLContains(page *rod.Page, substr string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if res, err := page.Eval(`() => location.href`); err == nil {
			if u := res.Value.Str(); strings.Contains(u, substr) {
				return u, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", fmt.Errorf("timeout waiting for URL containing %q", substr)
}

// waitURLChanges polls location.href until it differs from the current value.
func waitURLChanges(page *rod.Page, timeout time.Duration) (string, error) {
	start, err := page.Eval(`() => location.href`)
	if err != nil {
		return "", err
	}
	startURL := start.Value.Str()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if res, err := page.Eval(`() => location.href`); err == nil {
			if u := res.Value.Str(); u != startURL {
				return u, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", fmt.Errorf("timeout waiting for navigation (still at %s)", startURL)
}

// cmdHeaders sets/lists/clears extra HTTP headers persisted in the session
// state and applied by every subsequent rodney command.
func cmdHeaders(args []string) {
	clear := false
	var pairs []string
	for _, a := range args {
		if a == "--clear" {
			clear = true
		} else {
			pairs = append(pairs, a)
		}
	}
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	if clear {
		s.Headers = nil
		if err := saveState(s); err != nil {
			fatal("failed to save state: %v", err)
		}
		fmt.Println("Extra headers cleared")
		return
	}
	if len(pairs) == 0 {
		if len(s.Headers) == 0 {
			fmt.Println("(no extra headers set)")
			return
		}
		keys := make([]string, 0, len(s.Headers))
		for k := range s.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s: %s\n", k, s.Headers[k])
		}
		return
	}
	if s.Headers == nil {
		s.Headers = map[string]string{}
	}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			fatal("invalid header %q (expected name=value)", p)
		}
		s.Headers[k] = v
	}
	if err := saveState(s); err != nil {
		fatal("failed to save state: %v", err)
	}
	fmt.Printf("Headers set (%d total — applied to every request from now on)\n", len(s.Headers))
}

// cmdResource prints the cached response body of an already-loaded resource.
// Exact URL match first, then substring match against the resource tree.
func cmdResource(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney resource <url> [file|-]")
	}
	_, _, page := withPage()
	url := args[0]

	data, err := page.GetResource(url)
	if err != nil {
		// Substring match against all loaded resources
		tree, terr := proto.PageGetResourceTree{}.Call(page)
		if terr == nil {
			var walk func(ft *proto.PageFrameResourceTree) bool
			walk = func(ft *proto.PageFrameResourceTree) bool {
				for _, r := range ft.Resources {
					if strings.Contains(r.URL, url) {
						data, err = page.GetResource(r.URL)
						return true
					}
				}
				for _, c := range ft.ChildFrames {
					if walk(c) {
						return true
					}
				}
				return false
			}
			walk(tree.FrameTree)
		}
		if err != nil {
			fatal("resource not loaded (open the page first): %v", err)
		}
	}

	out := "-"
	if len(args) > 1 {
		out = args[1]
	}
	if out == "-" {
		os.Stdout.Write(data)
	} else if err := os.WriteFile(out, data, 0644); err != nil {
		fatal("failed to write file: %v", err)
	} else {
		fmt.Printf("Saved %d bytes to %s\n", len(data), out)
	}
}

// cmdHistory prints the navigation history of the active page.
func cmdHistory(args []string) {
	_, _, page := withPage()
	h, err := proto.PageGetNavigationHistory{}.Call(page)
	if err != nil {
		fatal("failed to get history: %v", err)
	}
	for i, e := range h.Entries {
		marker := " "
		if i == h.CurrentIndex {
			marker = "*"
		}
		fmt.Printf("%s [%d] %s\n", marker, i, e.URL)
	}
}

// elementCenter returns the center point of an element's bounding quad.
func elementCenter(el *rod.Element) (proto.Point, error) {
	shape, err := el.Shape()
	if err != nil || len(shape.Quads) == 0 {
		return proto.Point{}, fmt.Errorf("failed to get element shape: %w", err)
	}
	q := shape.Quads[0] // [x1,y1,x2,y2,x3,y3,x4,y4]
	var cx, cy float64
	for i := 0; i < 4; i++ {
		cx += q[i*2] / 4
		cy += q[i*2+1] / 4
	}
	return proto.Point{X: cx, Y: cy}, nil
}

// cmdDrag drags the source element onto the target element via real mouse
// events: move to source, button down, move linearly to target, button up.
func cmdDrag(args []string) {
	if len(args) != 2 {
		fatal("usage: rodney drag <source-selector> <target-selector>")
	}
	_, _, page := withPage()
	src, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("source not found: %v", err)
	}
	dst, err := pageElShadow(page, args[1])
	if err != nil {
		fatal("target not found: %v", err)
	}
	if err := src.ScrollIntoView(); err != nil {
		fatal("scroll to source failed: %v", err)
	}
	sp, err := elementCenter(src)
	if err != nil {
		fatal("%v", err)
	}
	if err := dst.ScrollIntoView(); err != nil {
		fatal("scroll to target failed: %v", err)
	}
	tp, err := elementCenter(dst)
	if err != nil {
		fatal("%v", err)
	}
	if err := page.Mouse.MoveTo(sp); err != nil {
		fatal("move to source failed: %v", err)
	}
	if err := page.Mouse.Down(proto.InputMouseButtonLeft, 1); err != nil {
		fatal("mouse down failed: %v", err)
	}
	if err := page.Mouse.MoveLinear(tp, 25); err != nil {
		fatal("move to target failed: %v", err)
	}
	if err := page.Mouse.Up(proto.InputMouseButtonLeft, 1); err != nil {
		fatal("mouse up failed: %v", err)
	}
	fmt.Printf("Dragged %s onto %s\n", args[0], args[1])
}

// cmdOnload manages JS that runs on every navigation (persisted in state,
// re-registered on each connection because EvalOnNewDocument is
// session-scoped).
func cmdOnload(args []string) {
	clear := false
	var js []string
	for _, a := range args {
		if a == "--clear" {
			clear = true
		} else {
			js = append(js, a)
		}
	}
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	if clear {
		s.Onload = nil
		if err := saveState(s); err != nil {
			fatal("failed to save state: %v", err)
		}
		fmt.Println("Onload scripts cleared")
		return
	}
	if len(js) == 0 {
		if len(s.Onload) == 0 {
			fmt.Println("(no onload scripts)")
			return
		}
		for i, j := range s.Onload {
			fmt.Printf("[%d] %s\n", i, j)
		}
		return
	}
	s.Onload = append(s.Onload, strings.Join(js, " "))
	if err := saveState(s); err != nil {
		fatal("failed to save state: %v", err)
	}
	fmt.Printf("Onload script added [%d total — runs on every navigation]\n", len(s.Onload))
}

// cmdWaitPage waits until a NEW page/tab opens (window.open, target=_blank)
// and switches the active page to it.
func cmdWaitPage(args []string) {
	timeout := defaultTimeout
	if len(args) > 0 {
		secs, err := strconv.ParseFloat(args[0], 64)
		if err != nil || secs < 0 {
			fatal("invalid timeout: %s", args[0])
		}
		timeout = time.Duration(secs * float64(time.Second))
	}
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	before, err := browser.Pages()
	if err != nil {
		fatal("failed to list pages: %v", err)
	}
	known := map[proto.TargetTargetID]bool{}
	for _, p := range before {
		known[p.TargetID] = true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pages, err := browser.Pages()
		if err == nil {
			for i, p := range pages {
				if !known[p.TargetID] {
					s.ActivePage = i
					if err := saveState(s); err != nil {
						fatal("failed to save state: %v", err)
					}
					info, _ := p.Info()
					url := ""
					if info != nil {
						url = info.URL
					}
					fmt.Printf("New page [%d] (t:%s) %s\n", i, p.TargetID, url)
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	fatal("timeout waiting for a new page")
}

// cmdStopLoad stops the page's pending navigation and resource fetches.
func cmdStopLoad(args []string) {
	_, _, page := withPage()
	if err := page.StopLoading(); err != nil {
		fatal("failed to stop loading: %v", err)
	}
	fmt.Println("Loading stopped")
}

// cmdTap taps an element (touch semantics; pairs with `rodney device`).
func cmdTap(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney tap <selector>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	if err := el.Tap(); err != nil {
		fatal("tap failed: %v", err)
	}
	fmt.Println("Tapped")
}

// cmdXPathOf prints the XPath of an element — helps building xpath queries.
func cmdXPathOf(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney xpath-of <selector>")
	}
	_, _, page := withPage()
	el, err := pageElShadow(page, args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	xp, err := el.GetXPath(true)
	if err != nil {
		fatal("failed to compute xpath: %v", err)
	}
	fmt.Println(xp)
}

// cmdFileChooser arms file-chooser interception as a persistent foreground
// process: every file chooser opened by the page (click on <input type=file>
// etc.) gets the given path, until Ctrl+C.
func cmdFileChooser(args []string) {
	if len(args) < 1 {
		fatal("usage: rodney filechooser <path>")
	}
	path := args[0]
	if _, err := os.Stat(path); err != nil {
		fatal("file not found: %s", path)
	}
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	page, err := getActivePage(browser, s)
	if err != nil {
		fatal("%v", err)
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()
	fmt.Fprintf(os.Stderr, "(intercepting file choosers on the active page, answering with %s — Ctrl+C to stop)\n", path)
	for {
		set, err := page.HandleFileDialog()
		if err != nil {
			fatal("failed to arm file chooser: %v", err)
		}
		if err := set([]string{path}); err != nil {
			fmt.Fprintf(os.Stderr, "failed to set file: %v\n", err)
		} else {
			fmt.Printf("[filechooser] answered with %s\n", path)
		}
	}
}

// cmdMonitor serves rod's live debug web UI (pages, eval console, requests).
func cmdMonitor(args []string) {
	host := ":0"
	if len(args) > 0 {
		host = args[0]
	}
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	browser, err := connectBrowser(s)
	if err != nil {
		fatal("%v", err)
	}
	url := browser.ServeMonitor(host)
	fmt.Printf("Monitor UI: %s\n(Ctrl+C to stop)\n", url)
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()
	select {}
}

// cmdDoctor runs self-diagnostics: binary version, Chrome availability,
// ffmpeg, state health, connectivity. Exit 0 if nothing FAILed.
func cmdDoctor(args []string) {
	failed := 0
	check := func(name, detail string, ok bool) {
		status := "PASS"
		if !ok {
			status = "FAIL"
			failed++
		}
		fmt.Printf("%-4s %-22s %s\n", status, name, detail)
	}

	check("version", version, true)

	// Chrome detection: ROD_CHROME_BIN or rod-managed chromium
	if bin := os.Getenv("ROD_CHROME_BIN"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			check("chrome", bin+" (ROD_CHROME_BIN)", true)
		} else {
			check("chrome", bin+" not found (ROD_CHROME_BIN)", false)
		}
	} else if path, has := launcher.LookPath(); has {
		check("chrome", path+" (rod-managed)", true)
	} else {
		check("chrome", "not found — set ROD_CHROME_BIN or run any command once (rod downloads chromium)", false)
	}

	// ffmpeg for mp4 video output (optional)
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		check("ffmpeg", "available (mp4 video output)", true)
	} else {
		fmt.Printf("WARN ffmpeg                  not in PATH — stop-video falls back to GIF")
		fmt.Println()
	}

	// state health
	if s, err := loadState(); err == nil {
		check("state", stateDir()+" (exists)", true)
		if b, err := connectBrowser(s); err == nil {
			v, verr := b.Version()
			detail := "connected"
			if verr == nil {
				detail = "connected — " + v.Product
			}
			check("browser", detail, true)
		} else {
			fmt.Printf("WARN browser                 state exists but browser not responding (stale session? run: rodney stop)")
			fmt.Println()
		}
		check("incognito", fmt.Sprintf("Incognito=%v Headers=%d Onload=%d Device=%q Viewport=%dx%d",
			s.Incognito, len(s.Headers), len(s.Onload), s.DeviceName, s.ViewportW, s.ViewportH), true)
	} else {
		fmt.Printf("WARN state                   no session in %s (run: rodney start)", stateDir())
		fmt.Println()
	}

	// state dir writable
	if err := os.MkdirAll(stateDir(), 0755); err == nil {
		check("state-dir", stateDir()+" writable", true)
	} else {
		check("state-dir", stateDir()+" not writable", false)
	}

	if failed > 0 {
		fmt.Printf("\n%d check(s) failed\n", failed)
		os.Exit(2)
	}
	fmt.Println("\nAll checks passed")
}
