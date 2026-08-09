package main

import (
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

//go:embed help.txt
var helpText string

// version is the devskale fork version. Overridable via -ldflags "-X main.version=..."
// for tagged releases, but the default keeps fork builds distinguishable from
// upstream (which reports plain "dev").
var version = "0.6.1" // devskale fork

// scopeMode determines whether to use a local or global state directory.
type scopeMode int

const (
	scopeAuto   scopeMode = iota // auto-detect: local if .jodney/state.json exists in cwd, else global
	scopeLocal                   // force local (./.jodney/)
	scopeGlobal                  // force global (~/.jodney/)
)

// activeStateDir is set once at startup based on --local/--global flags.
var activeStateDir string

// extractScopeArgs scans args for --local/--global, removes them, and returns the mode.
// If both appear, the last one wins.
func extractScopeArgs(args []string) (scopeMode, []string) {
	mode := scopeAuto
	var filtered []string
	for _, arg := range args {
		switch arg {
		case "--local":
			mode = scopeLocal
		case "--global":
			mode = scopeGlobal
		default:
			filtered = append(filtered, arg)
		}
	}
	return mode, filtered
}

// resolveStateDir determines the state directory based on scope mode and working directory.
func resolveStateDir(mode scopeMode, workingDir string) string {
	switch mode {
	case scopeLocal:
		return filepath.Join(workingDir, ".jodney")
	case scopeGlobal:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".jodney")
	default: // scopeAuto
		localDir := filepath.Join(workingDir, ".jodney")
		if _, err := os.Stat(filepath.Join(localDir, "state.json")); err == nil {
			return localDir
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".jodney")
	}
}

// State persisted between CLI invocations
type State struct {
	DebugURL       string `json:"debug_url"`
	ChromePID      int    `json:"chrome_pid"`
	ActivePage     int    `json:"active_page"` // index into pages list
	DataDir        string `json:"data_dir"`
	ProxyPID       int    `json:"proxy_pid,omitempty"`  // PID of auth proxy helper
	ProxyPort      int    `json:"proxy_port,omitempty"` // local port of auth proxy
	VideoRecording bool   `json:"video_recording,omitempty"`
	VideoDir       string `json:"video_dir,omitempty"`
}

// stateDirOverride allows tests to redirect state to a temp dir
var stateDirOverride string

func stateDir() string {
	if stateDirOverride != "" {
		return stateDirOverride
	}
	if dir := os.Getenv("JODNEY_HOME"); dir != "" {
		return dir
	}
	if activeStateDir != "" {
		return activeStateDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".jodney")
}

func statePath() string {
	return filepath.Join(stateDir(), "state.json")
}

// chromeBin returns the Chrome/Chromium binary path to use.
// Prefers JODNEY_CHROME_BIN, falling back to ROD_CHROME_BIN (the go-rod
// library convention) for compatibility. Returns "" if neither is set,
// meaning go-rod's default discovery is used.
func chromeBin() string {
	if bin := os.Getenv("JODNEY_CHROME_BIN"); bin != "" {
		return bin
	}
	return os.Getenv("ROD_CHROME_BIN")
}

func loadState() (*State, error) {
	data, err := os.ReadFile(statePath())
	if err != nil {
		return nil, fmt.Errorf("no browser session (run 'jodney start' first)")
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
// per-command `jodney <cmd> --help` output.
var commandUsage = map[string]string{
	"start":         "jodney start [--show] [--insecure|-k] [--local]",
	"connect":       "jodney connect <host:port>",
	"stop":          "jodney stop",
	"status":        "jodney status",
	"open":          "jodney open <url>",
	"back":          "jodney back",
	"forward":       "jodney forward",
	"reload":        "jodney reload [--hard]",
	"clear-cache":   "jodney clear-cache",
	"url":           "jodney url",
	"title":         "jodney title",
	"html":          "jodney html [selector]",
	"text":          "jodney text <selector>",
	"attr":          "jodney attr <selector> <attribute>",
	"pdf":           "jodney pdf [file]",
	"js":            "jodney js <expression>",
	"click":         "jodney click <selector>",
	"input":         "jodney input <selector> <text>",
	"clear":         "jodney clear <selector>",
	"select":        "jodney select <selector> <value>",
	"submit":        "jodney submit <selector>",
	"hover":         "jodney hover <selector>",
	"file":          "jodney file <selector> <path|->",
	"download":      "jodney download <selector> [file|-]",
	"focus":         "jodney focus <selector>",
	"wait":          "jodney wait <selector>",
	"waitload":      "jodney waitload",
	"waitstable":    "jodney waitstable",
	"waitidle":      "jodney waitidle",
	"sleep":         "jodney sleep <seconds>",
	"screenshot":    "jodney screenshot [-w N] [-h N] [file]",
	"screenshot-el": "jodney screenshot-el <selector> [file]",
	"start-video":   "jodney start-video",
	"stop-video":    "jodney stop-video [file]",
	"pages":         "jodney pages",
	"page":          "jodney page <index>",
	"newpage":       "jodney newpage [url]",
	"closepage":     "jodney closepage [index]",
	"exists":        "jodney exists <selector>",
	"count":         "jodney count <selector>",
	"visible":       "jodney visible <selector>",
	"assert":        "jodney assert <js-expression> [expected] [--message msg]",
	"ua":            "jodney ua <user-agent-string>",
	"timezone":      "jodney timezone <timezone-id>",
	"locale":        "jodney locale <locale>",
	"geo":           "jodney geo --lat <lat> --lon <lon>",
	"media":         "jodney media [--type T] [--feature name=value ...]",
	"ax-tree":       "jodney ax-tree [--depth N] [--json]",
	"ax-find":       "jodney ax-find [--name N] [--role R] [--json]",
	"ax-node":       "jodney ax-node <selector> [--json]",
	"cookie-set":    "jodney cookie-set <name> <value> [opts]",
	"cookie-get":    "jodney cookie-get [name] [--json]",
	"cookie-delete": "jodney cookie-delete <name> [opts]",
	"cookie-clear":  "jodney cookie-clear [--domain <domain>]",
	"mock":          "jodney mock <pattern> <response> [--status N] [--type MIME] [--method M]",
	"block":         "jodney block <pattern> [--method M]",
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
	mode, cleanedArgs := extractScopeArgs(os.Args[1:])
	if len(cleanedArgs) == 0 {
		printUsage()
		os.Exit(1)
	}

	wd, _ := os.Getwd()
	activeStateDir = resolveStateDir(mode, wd)

	cmd := cleanedArgs[0]
	args := cleanedArgs[1:]

	if cmd == "--version" {
		fmt.Println(version)
		os.Exit(0)
	}

	if cmd == "--update" {
		cmdUpdate(args)
	}

	// Per-command help: if the command is known and --help is present among
	// its args (or the command itself is a help request), print that command's
	// usage line instead of dispatching. This gives a uniform, friendly
	// `jodney <cmd> --help` for every command, including flag-based ones that
	// would otherwise choke on the flag parser. `-h` is intentionally NOT
	// treated as help here because some commands (e.g. screenshot) use it as a
	// flag alias.
	if cmdUsage, known := commandUsage[cmd]; known {
		if containsHelpFlag(args) {
			fmt.Printf("Usage: %s\n", cmdUsage)
			os.Exit(0)
		}
	}

	switch cmd {
	case "_proxy":
		cmdInternalProxy(args) // hidden: runs the auth proxy helper
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
	case "help", "-h", "--help":
		printUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(2)
	}
}

// Default timeout for element queries (seconds)
var defaultTimeout = 30 * time.Second

// timeoutSeconds returns the element-query default timeout in seconds.
// Prefers JODNEY_TIMEOUT, falling back to ROD_TIMEOUT (the go-rod library
// convention) for compatibility. Returns 0 if neither is set, meaning the
// package-level defaultTimeout is kept.
func timeoutSeconds() float64 {
	t := os.Getenv("JODNEY_TIMEOUT")
	if t == "" {
		t = os.Getenv("ROD_TIMEOUT")
	}
	secs, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0
	}
	return secs
}

func init() {
	if secs := timeoutSeconds(); secs > 0 {
		defaultTimeout = time.Duration(secs * float64(time.Second))
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
	// Start video capture if recording is active
	videoCleanup = maybeStartVideoCapture(page)
	return s, browser, page
}

// --- Commands ---

// parseStartArgs parses the flags for the "start" command.
// Returns ignoreCertErrors, headless, and an error for unknown flags.
func parseStartArgs(args []string) (ignoreCertErrors bool, headless bool, err error) {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&ignoreCertErrors, "insecure", false, "")
	fs.BoolVar(&ignoreCertErrors, "k", false, "")
	show := fs.Bool("show", false, "")

	if parseErr := fs.Parse(args); parseErr != nil {
		return false, true, fmt.Errorf("unknown flag: %s\nusage: jodney start [--show] [--insecure]", findUnknownFlag(args, fs))
	}
	if fs.NArg() > 0 {
		return false, true, fmt.Errorf("unknown flag: %s\nusage: jodney start [--show] [--insecure]", fs.Arg(0))
	}
	headless = !*show
	return ignoreCertErrors, headless, nil
}

// cmdUpdate rebuilds the jodney binary from upstream source. It clones the
// repo into a standard location if needed, pulls the latest, builds into the
// directory that currently holds the running binary, and exits.
//
// The binary is built from source (jodney is not published to PyPI), so this
// is the supported way to self-update. It requires git and Go on PATH.
func cmdUpdate(args []string) {
	// Resolve the path of the currently running binary.
	exe, err := os.Executable()
	if err != nil {
		fatal("cannot locate jodney binary: %v", err)
	}
	exe, _ = filepath.EvalSymlinks(exe)

	// The source repo lives next to the binary (repo/ = bin dir's parent's
	// child), or in a few well-known locations. Prefer a sibling repo dir.
	repo := ""
	candidates := []string{
		filepath.Join(filepath.Dir(exe), "..", "jodney"), // ~/.local/bin/../jodney
		filepath.Join(filepath.Dir(exe), "jodney"),       // same dir as binary
		filepath.Join(os.Getenv("HOME"), "src", "jodney"),
		filepath.Join(os.Getenv("HOME"), "code", "clones", "jodney"),
	}
	for _, c := range candidates {
		if fi, err := os.Stat(filepath.Join(c, "go.mod")); err == nil && !fi.IsDir() {
			repo = c
			break
		}
	}

	if repo == "" {
		// No existing checkout — clone into ~/src/jodney (the documented location).
		repo = filepath.Join(os.Getenv("HOME"), "src", "jodney")
		if err := os.MkdirAll(filepath.Dir(repo), 0755); err != nil {
			fatal("failed to create src dir: %v", err)
		}
		fmt.Printf("Cloning jodney into %s...\n", repo)
		out, err := exec.Command("git", "clone", "git@github.com:devskale/jodney.git", repo).CombinedOutput()
		if err != nil {
			fatal("git clone failed: %v\n%s", err, out)
		}
	} else {
		fmt.Printf("Updating %s...\n", repo)
		out, err := exec.Command("git", "-C", repo, "pull", "--ff-only").CombinedOutput()
		if err != nil {
			fatal("git pull failed (is the repo clean?): %v\n%s", err, out)
		}
	}

	// Build into the same directory as the current binary so PATH stays valid.
	dest := filepath.Join(filepath.Dir(exe), "jodney")
	fmt.Printf("Building %s...\n", dest)
	buildCmd := exec.Command("go", "build", "-o", dest, ".")
	buildCmd.Dir = repo
	out, err := buildCmd.CombinedOutput()
	if err != nil {
		fatal("go build failed: %v\n%s", err, out)
	}

	// Verify the new binary reports a version.
	newVer, _ := exec.Command(dest, "--version").Output()
	fmt.Printf("Updated to %s", newVer)
}

func cmdStart(args []string) {
	ignoreCertErrors, headless, err := parseStartArgs(args)
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
	os.MkdirAll(dataDir, 0755)

	l := launcher.New().
		Set("no-sandbox").
		Set("disable-gpu").
		Set("single-process"). // Required for screenshots in gVisor/container environments
		Leakless(false).       // Keep Chrome alive after CLI exits
		UserDataDir(dataDir).
		Headless(headless)

	// When in non-headless mode, make sure that we show the startup window immediately
	// (instead of showing a window only after calling "jodney open")
	if !headless {
		l = l.Delete("no-startup-window")
	}

	if bin := chromeBin(); bin != "" {
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
	}

	if err := saveState(state); err != nil {
		fatal("failed to save state: %v", err)
	}

	fmt.Printf("Chrome started (PID %d)\n", pid)
	fmt.Printf("Debug URL: %s\n", debugURL)
}

func cmdConnect(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney connect <host:port>")
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
	// Clean up any active video recording
	if s.VideoRecording && s.VideoDir != "" {
		os.RemoveAll(s.VideoDir)
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

func cmdOpen(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney open <url>")
	}
	url := args[0]
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
	_, _, page := withPage()
	if len(args) > 0 {
		el, err := page.Element(args[0])
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
		fatal("usage: jodney text <selector>")
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
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
		fatal("usage: jodney attr <selector> <attribute>")
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
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
	file := "page.pdf"
	if len(args) > 0 {
		file = args[0]
	}
	_, _, page := withPage()
	req := proto.PagePrintToPDF{}
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
		fatal("usage: jodney js <expression>")
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

func cmdClick(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney click <selector>")
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
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
		fatal("usage: jodney input <selector> <text>")
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	text := strings.Join(args[1:], " ")
	el.MustSelectAllText().MustInput(text)
	fmt.Printf("Typed: %s\n", text)
}

func cmdClear(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney clear <selector>")
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	el.MustSelectAllText().MustInput("")
	fmt.Println("Cleared")
}

func cmdFile(args []string) {
	if len(args) < 2 {
		fatal("usage: jodney file <selector> <path|->")
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
		tmp, err := os.CreateTemp("", "jodney-upload-*")
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
		fatal("usage: jodney download <selector> [file|-]")
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
// is interrupted (Ctrl+C / SIGTERM), so other jodney commands in separate
// shells can drive the browser while the mock is active.
//
// Usage:
//
//	jodney mock <pattern> <response> [--status N] [--type MIME] [--method M]
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
		fatal("usage: jodney mock <pattern> <response> [--status N] [--type MIME] [--method M]")
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
//	jodney block <pattern> [--method M]
func cmdBlock(args []string) {
	fs := flag.NewFlagSet("block", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	method := fs.String("method", "", "")

	if err := fs.Parse(args); err != nil {
		fatal("%v", err)
	}
	rest := fs.Args()
	if len(rest) < 1 {
		fatal("usage: jodney block <pattern> [--method M]")
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
		fatal("usage: jodney select <selector> <value>")
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
		fatal("usage: jodney submit <selector>")
	}
	_, _, page := withPage()
	_, err := page.Element(args[0])
	if err != nil {
		fatal("form not found: %v", err)
	}
	page.MustEval(fmt.Sprintf(`() => document.querySelector(%q).submit()`, args[0]))
	fmt.Println("Submitted")
}

func cmdHover(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney hover <selector>")
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	el.MustHover()
	fmt.Println("Hovered")
}

func cmdFocus(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney focus <selector>")
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
	if err != nil {
		fatal("element not found: %v", err)
	}
	el.MustFocus()
	fmt.Println("Focused")
}

func cmdWait(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney wait <selector>")
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
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
	_, _, page := withPage()
	page.MustWaitIdle()
	fmt.Println("Network idle")
}

func cmdSleep(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney sleep <seconds>")
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

	if err := fs.Parse(args); err != nil {
		fatal("%v", err)
	}

	fullPage := true
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "height" || f.Name == "h" {
			fullPage = false
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
		fatal("usage: jodney screenshot-el <selector> [file]")
	}
	file := "element.png"
	if len(args) > 1 {
		file = args[1]
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
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
		return nil, fmt.Errorf("video recording is not active (run 'jodney start-video' first)")
	}

	framesDir := s.VideoDir
	frameCount := countFrames(framesDir)

	result := &VideoResult{FrameCount: frameCount}

	// Assemble output if we have frames
	if frameCount > 0 && outputFile != "" {
		if strings.HasSuffix(strings.ToLower(outputFile), ".gif") {
			if gifResult, err := assembleGIF(framesDir, outputFile); err == nil {
				result.OutputFile = gifResult.OutputFile
				result.UniqueFrames = gifResult.UniqueFrames
			}
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
	for i, p := range pages {
		marker := " "
		if i == s.ActivePage {
			marker = "*"
		}
		info, _ := p.Info()
		if info != nil {
			fmt.Printf("%s [%d] %s - %s\n", marker, i, info.Title, info.URL)
		} else {
			fmt.Printf("%s [%d] (unknown)\n", marker, i)
		}
	}
}

func cmdPage(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney page <index>")
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil {
		fatal("invalid index: %v", err)
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
		idx, err = strconv.Atoi(args[0])
		if err != nil {
			fatal("invalid index: %v", err)
		}
	}
	if idx < 0 || idx >= len(pages) {
		fatal("page index %d out of range", idx)
	}

	pages[idx].MustClose()

	// Adjust active page
	if s.ActivePage >= len(pages)-1 {
		s.ActivePage = len(pages) - 2
	}
	if s.ActivePage < 0 {
		s.ActivePage = 0
	}
	saveState(s)
	fmt.Printf("Closed page %d\n", idx)
}

func cmdExists(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney exists <selector>")
	}
	_, _, page := withPage()
	has, _, err := page.Has(args[0])
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
		fatal("usage: jodney count <selector>")
	}
	_, _, page := withPage()
	els, err := page.Elements(args[0])
	if err != nil {
		fatal("query failed: %v", err)
	}
	fmt.Println(len(els))
}

func cmdVisible(args []string) {
	if len(args) < 1 {
		fatal("usage: jodney visible <selector>")
	}
	_, _, page := withPage()
	el, err := page.Element(args[0])
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
		fatal("usage: jodney assert <js-expression> [expected] [--message msg]")
	}

	expr, expected, message := parseAssertArgs(args)
	if expr == "" {
		fatal("usage: jodney assert <js-expression> [expected] [--message msg]")
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
		fatal("unknown flag: %s\nusage: jodney ax-tree [--depth N] [--json]", findUnknownFlag(args, fs))
	}
	if fs.NArg() > 0 {
		fatal("unknown flag: %s\nusage: jodney ax-tree [--depth N] [--json]", fs.Arg(0))
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
		fatal("unknown flag: %s\nusage: jodney ax-find [--name N] [--role R] [--json]", findUnknownFlag(args, fs))
	}
	if fs.NArg() > 0 {
		fatal("unknown flag: %s\nusage: jodney ax-find [--name N] [--role R] [--json]", fs.Arg(0))
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
		fatal("usage: jodney ax-node <selector> [--json]")
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
		fatal("usage: jodney ua <user-agent-string>")
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
		fatal("usage: jodney timezone <timezone-id>")
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
		fatal("usage: jodney locale <locale>")
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
			fatal("unknown flag: %s\nusage: jodney geo --lat <lat> --lon <lon>", args[i])
		}
	}

	if !hasLat || !hasLon {
		fatal("usage: jodney geo --lat <lat> --lon <lon>")
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
			fatal("unknown flag: %s\nusage: jodney media [--type T] [--feature name=value ...]", args[i])
		}
	}

	if mediaType == "" && len(features) == 0 {
		fatal("usage: jodney media [--type T] [--feature name=value ...]")
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
		return nil, fmt.Errorf("usage: jodney cookie-set <name> <value> --domain <domain> [options]")
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
		return "", "", "", "", fmt.Errorf("usage: jodney cookie-delete <name> [--domain <domain>] [--url <url>] [--path <path>]")
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
			fatal("unknown flag: %s\nusage: jodney cookie-clear [--domain <domain>]", args[i])
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

// cmdInternalProxy is a hidden subcommand: jodney _proxy <port> <upstream> <authHeader>
// It runs a local auth proxy that forwards to the upstream proxy with credentials.
func cmdInternalProxy(args []string) {
	if len(args) < 3 {
		fatal("usage: jodney _proxy <port> <upstream> <authHeader>")
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
