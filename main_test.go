package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/gif"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// testEnv holds a shared browser and test HTTP server for all tests.
type testEnv struct {
	browser *rod.Browser
	server  *httptest.Server
}

var env *testEnv

func TestMain(m *testing.M) {
	// Launch headless Chrome once for all tests
	l := launcher.New().
		Set("no-sandbox").
		Set("disable-gpu").
		Set("single-process").
		Headless(true).
		Leakless(false)

	if bin := os.Getenv("ROD_CHROME_BIN"); bin != "" {
		l = l.Bin(bin)
	}

	u := l.MustLaunch()
	browser := rod.New().ControlURL(u).MustConnect()

	// Start test HTTP server with known HTML fixtures
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/form", handleForm)
	mux.HandleFunc("/upload", handleUpload)
	mux.HandleFunc("/download", handleDownload)
	mux.HandleFunc("/testfile.txt", handleTestFile)
	mux.HandleFunc("/animated", handleAnimated)
	mux.HandleFunc("/empty", handleEmpty)
	server := httptest.NewServer(mux)

	env = &testEnv{browser: browser, server: server}

	code := m.Run()

	server.Close()
	browser.MustClose()
	os.Exit(code)
}

// --- HTML fixtures ---

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head><title>Test Page</title></head>
<body>
  <nav aria-label="Main">
    <a href="/about">About</a>
    <a href="/contact">Contact</a>
  </nav>
  <main>
    <h1>Welcome</h1>
    <p>Hello world</p>
    <button id="submit-btn">Submit</button>
    <button id="cancel-btn" disabled>Cancel</button>
  </main>
</body>
</html>`))
}

func handleForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head><title>Form Page</title></head>
<body>
  <h1>Contact Us</h1>
  <form>
    <label for="name-input">Name</label>
    <input id="name-input" type="text" aria-required="true">
    <label for="email-input">Email</label>
    <input id="email-input" type="email">
    <select id="topic" aria-label="Topic">
      <option value="general">General</option>
      <option value="support">Support</option>
    </select>
    <button type="submit">Send</button>
  </form>
</body>
</html>`))
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head><title>Upload Page</title></head>
<body>
  <input id="file-input" type="file" accept="image/*">
  <span id="file-name"></span>
  <script>
    document.getElementById('file-input').addEventListener('change', function(e) {
      document.getElementById('file-name').textContent = e.target.files[0] ? e.target.files[0].name : '';
    });
  </script>
</body>
</html>`))
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head><title>Download Page</title></head>
<body>
  <a id="file-link" href="/testfile.txt">Download file</a>
  <a id="data-link" href="data:text/plain;base64,SGVsbG8gV29ybGQ=">Download data</a>
  <img id="test-img" src="/testfile.txt">
</body>
</html>`))
}

func handleTestFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("Hello World"))
}

func handleAnimated(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html>
<html><head><style>
.box { width: 50px; height: 50px; background: red; animation: move 1s linear infinite; }
@keyframes move { 0% { margin-left: 0; } 100% { margin-left: 200px; } }
</style></head>
<body><div class="box"></div>
<div id="c">0</div>
<script>let n=0; setInterval(()=>{document.getElementById('c').textContent=++n;},100);</script>
</body></html>`))
}

func handleEmpty(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head><title>Empty Page</title></head>
<body></body>
</html>`))
}

// --- Helper: navigate to a fixture and return the page ---

func navigateTo(t *testing.T, path string) *rod.Page {
	t.Helper()
	page := env.browser.MustPage(env.server.URL + path)
	page.MustWaitLoad()
	t.Cleanup(func() { page.MustClose() })
	return page
}

// =====================
// ax-tree tests (RED)
// =====================

func TestAXTree_ReturnsNodes(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := proto.AccessibilityGetFullAXTree{}.Call(page)
	if err != nil {
		t.Fatalf("CDP call failed: %v", err)
	}
	// Sanity: we should get nodes back
	if len(result.Nodes) == 0 {
		t.Fatal("expected nodes in accessibility tree, got 0")
	}

	// Now test our formatting function
	out := formatAXTree(result.Nodes)
	if out == "" {
		t.Fatal("formatAXTree returned empty string")
	}
	if !strings.Contains(out, "Welcome") {
		t.Errorf("tree should contain heading text 'Welcome', got:\n%s", out)
	}
	if !strings.Contains(out, "button") {
		t.Errorf("tree should contain 'button' role, got:\n%s", out)
	}
	if !strings.Contains(out, "Submit") {
		t.Errorf("tree should contain button name 'Submit', got:\n%s", out)
	}
}

func TestAXTree_Indentation(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := proto.AccessibilityGetFullAXTree{}.Call(page)
	if err != nil {
		t.Fatalf("CDP call failed: %v", err)
	}
	out := formatAXTree(result.Nodes)
	lines := strings.Split(out, "\n")

	// Root node should have no indentation
	if len(lines) == 0 {
		t.Fatal("no lines in output")
	}
	if strings.HasPrefix(lines[0], " ") {
		t.Errorf("root node should not be indented, got: %q", lines[0])
	}

	// Some lines should be indented (children)
	hasIndented := false
	for _, line := range lines {
		if strings.HasPrefix(line, "  ") {
			hasIndented = true
			break
		}
	}
	if !hasIndented {
		t.Errorf("expected some indented lines for child nodes, got:\n%s", out)
	}
}

func TestAXTree_SkipsIgnoredNodes(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := proto.AccessibilityGetFullAXTree{}.Call(page)
	if err != nil {
		t.Fatalf("CDP call failed: %v", err)
	}
	out := formatAXTree(result.Nodes)

	// Count ignored vs total
	ignoredCount := 0
	for _, node := range result.Nodes {
		if node.Ignored {
			ignoredCount++
		}
	}

	// If there are ignored nodes, they shouldn't appear in text output
	if ignoredCount > 0 {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) >= len(result.Nodes) {
			t.Errorf("text output should skip ignored nodes: %d lines for %d nodes (%d ignored)",
				len(lines), len(result.Nodes), ignoredCount)
		}
	}
}

func TestAXTree_DepthLimit(t *testing.T) {
	page := navigateTo(t, "/")
	full, err := proto.AccessibilityGetFullAXTree{}.Call(page)
	if err != nil {
		t.Fatalf("CDP call failed: %v", err)
	}

	depth := 2
	limited, err := proto.AccessibilityGetFullAXTree{Depth: &depth}.Call(page)
	if err != nil {
		t.Fatalf("CDP call with depth failed: %v", err)
	}

	if len(limited.Nodes) >= len(full.Nodes) {
		t.Errorf("depth-limited tree (%d nodes) should have fewer nodes than full tree (%d nodes)",
			len(limited.Nodes), len(full.Nodes))
	}
}

func TestAXTree_JSONOutput(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := proto.AccessibilityGetFullAXTree{}.Call(page)
	if err != nil {
		t.Fatalf("CDP call failed: %v", err)
	}
	out := formatAXTreeJSON(result.Nodes)
	// Must be valid JSON
	var parsed []interface{}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("JSON output is not valid JSON: %v\nOutput:\n%s", err, out[:min(len(out), 500)])
	}
	if len(parsed) == 0 {
		t.Error("JSON output should contain nodes")
	}
}

// =====================
// ax-find tests (RED)
// =====================

func TestAXFind_ByRole(t *testing.T) {
	page := navigateTo(t, "/")
	nodes, err := queryAXNodes(page, "", "button")
	if err != nil {
		t.Fatalf("queryAXNodes failed: %v", err)
	}
	if len(nodes) < 2 {
		t.Fatalf("expected at least 2 buttons, got %d", len(nodes))
	}

	out := formatAXNodeList(nodes)
	if !strings.Contains(out, "Submit") {
		t.Errorf("output should contain 'Submit' button, got:\n%s", out)
	}
	if !strings.Contains(out, "Cancel") {
		t.Errorf("output should contain 'Cancel' button, got:\n%s", out)
	}
}

func TestAXFind_ByName(t *testing.T) {
	page := navigateTo(t, "/")
	nodes, err := queryAXNodes(page, "Submit", "")
	if err != nil {
		t.Fatalf("queryAXNodes failed: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("expected at least 1 node named 'Submit', got 0")
	}
	out := formatAXNodeList(nodes)
	if !strings.Contains(out, "Submit") {
		t.Errorf("output should contain 'Submit', got:\n%s", out)
	}
}

func TestAXFind_ByNameAndRoleExact(t *testing.T) {
	page := navigateTo(t, "/")
	// Combining name + role should give exactly one result
	nodes, err := queryAXNodes(page, "Submit", "button")
	if err != nil {
		t.Fatalf("queryAXNodes failed: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected exactly 1 button named 'Submit', got %d", len(nodes))
	}
}

func TestAXFind_ByNameAndRole(t *testing.T) {
	page := navigateTo(t, "/")
	nodes, err := queryAXNodes(page, "About", "link")
	if err != nil {
		t.Fatalf("queryAXNodes failed: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 link named 'About', got %d", len(nodes))
	}
}

func TestAXFind_NoResults(t *testing.T) {
	page := navigateTo(t, "/")
	nodes, err := queryAXNodes(page, "NonexistentThing", "")
	if err != nil {
		t.Fatalf("queryAXNodes failed: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("expected 0 results for nonexistent name, got %d", len(nodes))
	}
}

func TestAXFind_FormPage(t *testing.T) {
	page := navigateTo(t, "/form")
	nodes, err := queryAXNodes(page, "", "textbox")
	if err != nil {
		t.Fatalf("queryAXNodes failed: %v", err)
	}
	if len(nodes) < 2 {
		t.Fatalf("expected at least 2 textboxes on form page, got %d", len(nodes))
	}
}

// =====================
// ax-node tests (RED)
// =====================

func TestAXNode_ButtonBySelector(t *testing.T) {
	page := navigateTo(t, "/")
	node, err := getAXNode(page, "#submit-btn")
	if err != nil {
		t.Fatalf("getAXNode failed: %v", err)
	}
	out := formatAXNodeDetail(node)
	if !strings.Contains(out, "button") {
		t.Errorf("should show role 'button', got:\n%s", out)
	}
	if !strings.Contains(out, "Submit") {
		t.Errorf("should show name 'Submit', got:\n%s", out)
	}
}

func TestAXNode_DisabledButton(t *testing.T) {
	page := navigateTo(t, "/")
	node, err := getAXNode(page, "#cancel-btn")
	if err != nil {
		t.Fatalf("getAXNode failed: %v", err)
	}
	out := formatAXNodeDetail(node)
	if !strings.Contains(out, "button") {
		t.Errorf("should show role 'button', got:\n%s", out)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("should show disabled property, got:\n%s", out)
	}
}

func TestAXNode_InputWithLabel(t *testing.T) {
	page := navigateTo(t, "/form")
	node, err := getAXNode(page, "#name-input")
	if err != nil {
		t.Fatalf("getAXNode failed: %v", err)
	}
	out := formatAXNodeDetail(node)
	if !strings.Contains(out, "textbox") {
		t.Errorf("should show role 'textbox', got:\n%s", out)
	}
	if !strings.Contains(out, "Name") {
		t.Errorf("should show accessible name 'Name' from label, got:\n%s", out)
	}
}

func TestAXNode_HeadingLevel(t *testing.T) {
	page := navigateTo(t, "/")
	node, err := getAXNode(page, "h1")
	if err != nil {
		t.Fatalf("getAXNode failed: %v", err)
	}
	out := formatAXNodeDetail(node)
	if !strings.Contains(out, "heading") {
		t.Errorf("should show role 'heading', got:\n%s", out)
	}
	if !strings.Contains(out, "level") {
		t.Errorf("should show level property for heading, got:\n%s", out)
	}
}

func TestAXNode_JSONOutput(t *testing.T) {
	page := navigateTo(t, "/")
	node, err := getAXNode(page, "#submit-btn")
	if err != nil {
		t.Fatalf("getAXNode failed: %v", err)
	}
	out := formatAXNodeDetailJSON(node)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("JSON output is not valid JSON: %v\nOutput:\n%s", err, out)
	}
	if _, ok := parsed["nodeId"]; !ok {
		t.Error("JSON should contain nodeId field")
	}
}

func TestAXNode_SelectorNotFound(t *testing.T) {
	page := navigateTo(t, "/")
	// Use a short timeout so we don't block for 30s waiting for a nonexistent element
	shortPage := page.Timeout(2 * time.Second)
	_, err := getAXNode(shortPage, "#does-not-exist")
	if err == nil {
		t.Error("expected error for nonexistent selector, got nil")
	}
}

// =====================
// file command tests
// =====================

func TestFile_SetFileOnInput(t *testing.T) {
	page := navigateTo(t, "/upload")

	// Create a temp file to upload
	tmp, err := os.CreateTemp("", "jodney-test-*.txt")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmp.Name())
	tmp.Write([]byte("test content"))
	tmp.Close()

	el, err := page.Element("#file-input")
	if err != nil {
		t.Fatalf("element not found: %v", err)
	}
	if err := el.SetFiles([]string{tmp.Name()}); err != nil {
		t.Fatalf("SetFiles failed: %v", err)
	}

	// Wait for the change event to fire and check the file name
	page.MustWaitStable()
	nameEl, err := page.Element("#file-name")
	if err != nil {
		t.Fatalf("file-name element not found: %v", err)
	}
	text, _ := nameEl.Text()
	if text == "" {
		t.Error("expected file name to be set after SetFiles, got empty string")
	}
}

func TestFile_MultipleFiles(t *testing.T) {
	page := navigateTo(t, "/upload")

	tmp1, _ := os.CreateTemp("", "jodney-test1-*.txt")
	defer os.Remove(tmp1.Name())
	tmp1.Write([]byte("file 1"))
	tmp1.Close()

	tmp2, _ := os.CreateTemp("", "jodney-test2-*.txt")
	defer os.Remove(tmp2.Name())
	tmp2.Write([]byte("file 2"))
	tmp2.Close()

	el, err := page.Element("#file-input")
	if err != nil {
		t.Fatalf("element not found: %v", err)
	}

	// Setting files should not error even with multiple files
	if err := el.SetFiles([]string{tmp1.Name(), tmp2.Name()}); err != nil {
		t.Fatalf("SetFiles with multiple files failed: %v", err)
	}
}

// =====================
// download command tests
// =====================

func TestDownload_DataURL(t *testing.T) {
	// Test decoding a data: URL directly
	data, err := decodeDataURL("data:text/plain;base64,SGVsbG8gV29ybGQ=")
	if err != nil {
		t.Fatalf("decodeDataURL failed: %v", err)
	}
	if string(data) != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", string(data))
	}
}

func TestDownload_DataURL_URLEncoded(t *testing.T) {
	data, err := decodeDataURL("data:text/plain,Hello%20World")
	if err != nil {
		t.Fatalf("decodeDataURL failed: %v", err)
	}
	if string(data) != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", string(data))
	}
}

func TestDownload_InferFilename_URL(t *testing.T) {
	name := inferDownloadFilename("https://example.com/images/photo.png")
	if name != "photo.png" {
		t.Errorf("expected 'photo.png', got %q", name)
	}
}

func TestDownload_InferFilename_DataURL(t *testing.T) {
	name := inferDownloadFilename("data:image/png;base64,abc")
	if !strings.HasPrefix(name, "download") || !strings.Contains(name, ".png") {
		t.Errorf("expected 'download*.png', got %q", name)
	}
}

func TestDownload_FetchLink(t *testing.T) {
	page := navigateTo(t, "/download")

	el, err := page.Element("#file-link")
	if err != nil {
		t.Fatalf("element not found: %v", err)
	}
	href := el.MustAttribute("href")
	if href == nil {
		t.Fatal("expected href attribute")
	}

	// Fetch using JS in the page context, same as cmdDownload does
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
	}`, *href)
	result, err := page.Eval(js)
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}

	data, err := base64.StdEncoding.DecodeString(result.Value.Str())
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	if string(data) != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", string(data))
	}
}

func TestDownload_DataLinkElement(t *testing.T) {
	page := navigateTo(t, "/download")

	el, err := page.Element("#data-link")
	if err != nil {
		t.Fatalf("element not found: %v", err)
	}
	href := el.MustAttribute("href")
	if href == nil {
		t.Fatal("expected href attribute")
	}

	data, err := decodeDataURL(*href)
	if err != nil {
		t.Fatalf("decodeDataURL failed: %v", err)
	}
	if string(data) != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", string(data))
	}
}

func TestDownload_ImgSrc(t *testing.T) {
	page := navigateTo(t, "/download")

	el, err := page.Element("#test-img")
	if err != nil {
		t.Fatalf("element not found: %v", err)
	}
	src := el.MustAttribute("src")
	if src == nil {
		t.Fatal("expected src attribute")
	}
	if *src != "/testfile.txt" {
		t.Errorf("expected '/testfile.txt', got %q", *src)
	}
}

// =====================
// Directory-scoped sessions tests
// =====================

func TestExtractScopeArgs_NoFlags(t *testing.T) {
	mode, remaining := extractScopeArgs([]string{"open", "https://example.com"})
	if mode != scopeAuto {
		t.Errorf("expected scopeAuto, got %v", mode)
	}
	if len(remaining) != 2 || remaining[0] != "open" || remaining[1] != "https://example.com" {
		t.Errorf("expected [open https://example.com], got %v", remaining)
	}
}

func TestExtractScopeArgs_LocalFlag(t *testing.T) {
	mode, remaining := extractScopeArgs([]string{"--local", "start"})
	if mode != scopeLocal {
		t.Errorf("expected scopeLocal, got %v", mode)
	}
	if len(remaining) != 1 || remaining[0] != "start" {
		t.Errorf("expected [start], got %v", remaining)
	}
}

func TestExtractScopeArgs_GlobalFlag(t *testing.T) {
	mode, remaining := extractScopeArgs([]string{"--global", "open", "https://example.com"})
	if mode != scopeGlobal {
		t.Errorf("expected scopeGlobal, got %v", mode)
	}
	if len(remaining) != 2 || remaining[0] != "open" || remaining[1] != "https://example.com" {
		t.Errorf("expected [open https://example.com], got %v", remaining)
	}
}

func TestExtractScopeArgs_LocalFlagAfterCommand(t *testing.T) {
	mode, remaining := extractScopeArgs([]string{"open", "--local", "https://example.com"})
	if mode != scopeLocal {
		t.Errorf("expected scopeLocal, got %v", mode)
	}
	if len(remaining) != 2 || remaining[0] != "open" || remaining[1] != "https://example.com" {
		t.Errorf("expected [open https://example.com], got %v", remaining)
	}
}

func TestExtractScopeArgs_LastFlagWins(t *testing.T) {
	mode, _ := extractScopeArgs([]string{"--local", "--global", "start"})
	if mode != scopeGlobal {
		t.Errorf("expected last flag (scopeGlobal) to win, got %v", mode)
	}
}

func TestResolveStateDir_Global(t *testing.T) {
	dir := resolveStateDir(scopeGlobal, "/some/working/dir")
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".jodney")
	if dir != expected {
		t.Errorf("expected %q, got %q", expected, dir)
	}
}

func TestResolveStateDir_Local(t *testing.T) {
	dir := resolveStateDir(scopeLocal, "/some/working/dir")
	expected := filepath.Join("/some/working/dir", ".jodney")
	if dir != expected {
		t.Errorf("expected %q, got %q", expected, dir)
	}
}

func TestResolveStateDir_AutoPrefersLocal(t *testing.T) {
	// Create a temp directory with a .jodney/state.json to simulate local session
	tmpDir := t.TempDir()
	localJodney := filepath.Join(tmpDir, ".jodney")
	os.MkdirAll(localJodney, 0755)
	os.WriteFile(filepath.Join(localJodney, "state.json"), []byte(`{}`), 0644)

	dir := resolveStateDir(scopeAuto, tmpDir)
	if dir != localJodney {
		t.Errorf("auto mode should prefer local when .jodney/state.json exists: expected %q, got %q", localJodney, dir)
	}
}

func TestResolveStateDir_AutoFallsBackToGlobal(t *testing.T) {
	// Use a temp directory with NO .jodney/ — should fall back to global
	tmpDir := t.TempDir()
	dir := resolveStateDir(scopeAuto, tmpDir)
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".jodney")
	if dir != expected {
		t.Errorf("auto mode should fall back to global: expected %q, got %q", expected, dir)
	}
}

func TestResolveStateDir_LocalUsesWorkingDir(t *testing.T) {
	tmpDir := t.TempDir()
	dir := resolveStateDir(scopeLocal, tmpDir)
	expected := filepath.Join(tmpDir, ".jodney")
	if dir != expected {
		t.Errorf("local mode should use working dir: expected %q, got %q", expected, dir)
	}
}

// =====================
// JODNEY_HOME env var tests
// =====================

func TestStateDir_Default(t *testing.T) {
	t.Setenv("JODNEY_HOME", "")
	home, _ := os.UserHomeDir()
	want := home + "/.jodney"
	got := stateDir()
	if got != want {
		t.Errorf("stateDir() = %q, want %q", got, want)
	}
}

func TestStateDir_EnvVar(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("JODNEY_HOME", dir)
	got := stateDir()
	if got != dir {
		t.Errorf("stateDir() = %q, want %q", got, dir)
	}
}

func TestMimeToExt(t *testing.T) {
	tests := []struct {
		mime string
		ext  string
	}{
		{"image/png", ".png"},
		{"image/jpeg", ".jpg"},
		{"application/pdf", ".pdf"},
		{"text/plain", ".txt"},
		{"unknown/type", ""},
	}
	for _, tt := range tests {
		got := mimeToExt(tt.mime)
		if got != tt.ext {
			t.Errorf("mimeToExt(%q) = %q, want %q", tt.mime, got, tt.ext)
		}
	}
}

// =====================
// assert command tests
// =====================

func TestAssert_TruthyPass_String(t *testing.T) {
	page := navigateTo(t, "/")
	// document.title is "Test Page" which is truthy
	result, err := page.Eval(`() => { return (document.title); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	raw := result.Value.JSON("", "")
	// Should not be falsy
	switch raw {
	case "false", "0", "null", "undefined", `""`:
		t.Errorf("document.title should be truthy, got raw=%q", raw)
	}
	if result.Value.Str() != "Test Page" {
		t.Errorf("expected 'Test Page', got %q", result.Value.Str())
	}
}

func TestAssert_TruthyPass_True(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (1 === 1); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	raw := result.Value.JSON("", "")
	if raw != "true" {
		t.Errorf("1 === 1 should be true, got %q", raw)
	}
}

func TestAssert_TruthyPass_Number(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (42); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	raw := result.Value.JSON("", "")
	if raw == "0" || raw == "false" || raw == "null" || raw == "undefined" || raw == `""` {
		t.Errorf("42 should be truthy, got raw=%q", raw)
	}
}

func TestAssert_TruthyFail_Null(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (document.querySelector(".nonexistent")); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	raw := result.Value.JSON("", "")
	if raw != "null" {
		t.Errorf("querySelector for nonexistent should return null, got %q", raw)
	}
}

func TestAssert_TruthyFail_False(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (false); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	raw := result.Value.JSON("", "")
	if raw != "false" {
		t.Errorf("false should be false, got %q", raw)
	}
}

func TestAssert_TruthyFail_Zero(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (0); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	raw := result.Value.JSON("", "")
	if raw != "0" {
		t.Errorf("0 should be 0, got %q", raw)
	}
}

func TestAssert_TruthyFail_EmptyString(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (""); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	raw := result.Value.JSON("", "")
	if raw != `""` {
		t.Errorf("empty string should have JSON repr '\"\"', got %q", raw)
	}
}

func TestAssert_EqualityPass_Title(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (document.title); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	actual := result.Value.Str()
	if actual != "Test Page" {
		t.Errorf("expected 'Test Page', got %q", actual)
	}
}

func TestAssert_EqualityPass_Count(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (document.querySelectorAll("button").length); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	raw := result.Value.JSON("", "")
	if raw != "2" {
		t.Errorf("expected 2 buttons, got %q", raw)
	}
}

func TestAssert_EqualityFail_WrongTitle(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (document.title); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	actual := result.Value.Str()
	if actual == "Wrong Title" {
		t.Error("title should NOT equal 'Wrong Title'")
	}
}

func TestAssert_EqualityPass_BoolString(t *testing.T) {
	page := navigateTo(t, "/")
	result, err := page.Eval(`() => { return (1 === 1); }`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	raw := result.Value.JSON("", "")
	if raw != "true" {
		t.Errorf("1 === 1 should produce 'true', got %q", raw)
	}
}

func TestAssert_ValueFormatting_MatchesJSCommand(t *testing.T) {
	// Verify that the value formatting used by assert matches what jodney js outputs
	page := navigateTo(t, "/")

	tests := []struct {
		expr     string
		expected string
	}{
		{`document.title`, "Test Page"}, // string unquoted
		{`1 + 2`, "3"},                  // number
		{`true`, "true"},                // boolean
		{`null`, "null"},                // null
		{`document.querySelectorAll("button").length`, "2"}, // number from DOM
	}

	for _, tt := range tests {
		js := fmt.Sprintf(`() => { return (%s); }`, tt.expr)
		result, err := page.Eval(js)
		if err != nil {
			t.Fatalf("eval %q failed: %v", tt.expr, err)
		}

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

		if actual != tt.expected {
			t.Errorf("expr %q: expected %q, got %q (raw=%q)", tt.expr, tt.expected, actual, raw)
		}
	}
}

// =====================
// assert --message tests
// =====================

func TestParseAssertArgs_ExprOnly(t *testing.T) {
	expr, expected, message := parseAssertArgs([]string{"document.title"})
	if expr != "document.title" {
		t.Errorf("expr = %q, want %q", expr, "document.title")
	}
	if expected != nil {
		t.Errorf("expected should be nil, got %q", *expected)
	}
	if message != "" {
		t.Errorf("message should be empty, got %q", message)
	}
}

func TestParseAssertArgs_ExprAndExpected(t *testing.T) {
	expr, expected, message := parseAssertArgs([]string{"document.title", "Dashboard"})
	if expr != "document.title" {
		t.Errorf("expr = %q, want %q", expr, "document.title")
	}
	if expected == nil || *expected != "Dashboard" {
		t.Errorf("expected = %v, want %q", expected, "Dashboard")
	}
	if message != "" {
		t.Errorf("message should be empty, got %q", message)
	}
}

func TestParseAssertArgs_MessageLong(t *testing.T) {
	expr, expected, message := parseAssertArgs([]string{"document.title", "--message", "Page title check"})
	if expr != "document.title" {
		t.Errorf("expr = %q, want %q", expr, "document.title")
	}
	if expected != nil {
		t.Errorf("expected should be nil for truthy with --message, got %q", *expected)
	}
	if message != "Page title check" {
		t.Errorf("message = %q, want %q", message, "Page title check")
	}
}

func TestParseAssertArgs_MessageShort(t *testing.T) {
	expr, expected, message := parseAssertArgs([]string{"document.title", "-m", "Title check"})
	if expr != "document.title" {
		t.Errorf("expr = %q, want %q", expr, "document.title")
	}
	if expected != nil {
		t.Errorf("expected should be nil, got %q", *expected)
	}
	if message != "Title check" {
		t.Errorf("message = %q, want %q", message, "Title check")
	}
}

func TestParseAssertArgs_EqualityWithMessage(t *testing.T) {
	expr, expected, message := parseAssertArgs([]string{"document.title", "Dashboard", "--message", "Wrong page"})
	if expr != "document.title" {
		t.Errorf("expr = %q, want %q", expr, "document.title")
	}
	if expected == nil || *expected != "Dashboard" {
		t.Errorf("expected = %v, want %q", expected, "Dashboard")
	}
	if message != "Wrong page" {
		t.Errorf("message = %q, want %q", message, "Wrong page")
	}
}

func TestParseAssertArgs_MessageBeforeExpr(t *testing.T) {
	// --message can appear anywhere; positional args still work
	expr, expected, message := parseAssertArgs([]string{"-m", "Check", "document.title", "Home"})
	if expr != "document.title" {
		t.Errorf("expr = %q, want %q", expr, "document.title")
	}
	if expected == nil || *expected != "Home" {
		t.Errorf("expected = %v, want %q", expected, "Home")
	}
	if message != "Check" {
		t.Errorf("message = %q, want %q", message, "Check")
	}
}

func TestFormatAssertFail_TruthyNoMessage(t *testing.T) {
	got := formatAssertFail("null", nil, "")
	if got != "fail: got null" {
		t.Errorf("got %q, want %q", got, "fail: got null")
	}
}

func TestFormatAssertFail_TruthyWithMessage(t *testing.T) {
	got := formatAssertFail("null", nil, "User should be logged in")
	if got != "fail: User should be logged in (got null)" {
		t.Errorf("got %q, want %q", got, "fail: User should be logged in (got null)")
	}
}

func TestFormatAssertFail_EqualityNoMessage(t *testing.T) {
	expected := "Dashboard"
	got := formatAssertFail("Task Tracker", &expected, "")
	want := `fail: got "Task Tracker", expected "Dashboard"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatAssertFail_EqualityWithMessage(t *testing.T) {
	expected := "Dashboard"
	got := formatAssertFail("Task Tracker", &expected, "Wrong page loaded")
	want := `fail: Wrong page loaded (got "Task Tracker", expected "Dashboard")`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// =====================
// parseStartArgs tests
// =====================

func TestParseStartArgs_NoFlags(t *testing.T) {
	insecure, headless, err := parseStartArgs([]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if insecure {
		t.Error("expected insecure=false with no flags")
	}
	if !headless {
		t.Error("expected headless=true with no flags")
	}
}

func TestParseStartArgs_ShowFlag(t *testing.T) {
	insecure, headless, err := parseStartArgs([]string{"--show"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if insecure {
		t.Error("expected insecure=false")
	}
	if headless {
		t.Error("expected headless=false when --show is passed")
	}
}

func TestParseStartArgs_InsecureFlag(t *testing.T) {
	insecure, headless, err := parseStartArgs([]string{"--insecure"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !insecure {
		t.Error("expected insecure=true when --insecure is passed")
	}
	if !headless {
		t.Error("expected headless=true when only --insecure is passed")
	}
}

func TestParseStartArgs_InsecureShortFlag(t *testing.T) {
	insecure, _, err := parseStartArgs([]string{"-k"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !insecure {
		t.Error("expected insecure=true when -k is passed")
	}
}

func TestParseStartArgs_ShowAndInsecure(t *testing.T) {
	insecure, headless, err := parseStartArgs([]string{"--show", "--insecure"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !insecure {
		t.Error("expected insecure=true")
	}
	if headless {
		t.Error("expected headless=false when --show is passed")
	}
}

func TestParseStartArgs_UnknownFlag(t *testing.T) {
	_, _, err := parseStartArgs([]string{"--bogus"})
	if err == nil {
		t.Fatal("expected error for unknown flag --bogus")
	}
	if !strings.Contains(err.Error(), "--bogus") {
		t.Errorf("error should mention the unknown flag, got: %v", err)
	}
}

// ua (user agent) tests
// =====================

func TestUA_OverridesUserAgent(t *testing.T) {
	page := navigateTo(t, "/")
	newUA := "Googlebot/2.1 (+http://www.google.com/bot.html)"
	err := applyUserAgent(page, newUA)
	if err != nil {
		t.Fatalf("applyUserAgent failed: %v", err)
	}
	// Reload so the new UA takes effect on the next request
	page.MustReload()
	page.MustWaitLoad()
	// Check via JS that navigator.userAgent reflects the override
	result, err := page.Eval(`() => navigator.userAgent`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	got := result.Value.Str()
	if got != newUA {
		t.Errorf("expected userAgent %q, got %q", newUA, got)
	}
}

func TestUA_EmptyStringResetsToDefault(t *testing.T) {
	page := navigateTo(t, "/")
	// First get the default user agent
	defaultResult, err := page.Eval(`() => navigator.userAgent`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	defaultUA := defaultResult.Value.Str()

	// Override to something custom
	err = applyUserAgent(page, "CustomBot/1.0")
	if err != nil {
		t.Fatalf("applyUserAgent failed: %v", err)
	}
	page.MustReload()
	page.MustWaitLoad()

	// Reset by setting empty UA (which should restore default behavior)
	// The CDP spec says setting empty string resets to default
	err = applyUserAgent(page, "")
	if err != nil {
		t.Fatalf("applyUserAgent with empty string failed: %v", err)
	}
	page.MustReload()
	page.MustWaitLoad()

	result, err := page.Eval(`() => navigator.userAgent`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	got := result.Value.Str()
	if got == "CustomBot/1.0" {
		t.Errorf("UA should have been reset, still got %q", got)
	}
	_ = defaultUA // default UA may or may not match exactly after reset
}

// =====================
// timezone tests
// =====================

func TestTimezone_OverridesTimezone(t *testing.T) {
	page := navigateTo(t, "/")
	err := applyTimezone(page, "Asia/Tokyo")
	if err != nil {
		t.Fatalf("applyTimezone failed: %v", err)
	}
	// Check the timezone via JS
	result, err := page.Eval(`() => Intl.DateTimeFormat().resolvedOptions().timeZone`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	got := result.Value.Str()
	if got != "Asia/Tokyo" {
		t.Errorf("expected timezone 'Asia/Tokyo', got %q", got)
	}
}

func TestTimezone_DifferentTimezone(t *testing.T) {
	page := navigateTo(t, "/")
	err := applyTimezone(page, "America/New_York")
	if err != nil {
		t.Fatalf("applyTimezone failed: %v", err)
	}
	result, err := page.Eval(`() => Intl.DateTimeFormat().resolvedOptions().timeZone`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	got := result.Value.Str()
	if got != "America/New_York" {
		t.Errorf("expected timezone 'America/New_York', got %q", got)
	}
}

func TestTimezone_InvalidTimezoneReturnsError(t *testing.T) {
	page := navigateTo(t, "/")
	err := applyTimezone(page, "Not/A/Real/Timezone")
	if err == nil {
		t.Error("expected error for invalid timezone, got nil")
	}
}

// =====================
// locale tests
// =====================

func TestLocale_OverridesLocale(t *testing.T) {
	page := navigateTo(t, "/")
	err := applyLocale(page, "de-DE")
	if err != nil {
		t.Fatalf("applyLocale failed: %v", err)
	}
	result, err := page.Eval(`() => Intl.DateTimeFormat().resolvedOptions().locale`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	got := result.Value.Str()
	if got != "de-DE" {
		t.Errorf("expected locale 'de-DE', got %q", got)
	}
}

func TestLocale_JapaneseLocale(t *testing.T) {
	page := navigateTo(t, "/")
	err := applyLocale(page, "ja-JP")
	if err != nil {
		t.Fatalf("applyLocale failed: %v", err)
	}
	result, err := page.Eval(`() => Intl.DateTimeFormat().resolvedOptions().locale`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	got := result.Value.Str()
	if got != "ja-JP" {
		t.Errorf("expected locale 'ja-JP', got %q", got)
	}
}

func TestLocale_EmptyStringResets(t *testing.T) {
	page := navigateTo(t, "/")
	// Override to German
	err := applyLocale(page, "de-DE")
	if err != nil {
		t.Fatalf("applyLocale failed: %v", err)
	}
	// Reset by passing empty
	err = applyLocale(page, "")
	if err != nil {
		t.Fatalf("applyLocale with empty string failed: %v", err)
	}
	// Should no longer be de-DE after reset
	result, err := page.Eval(`() => Intl.DateTimeFormat().resolvedOptions().locale`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	got := result.Value.Str()
	if got == "de-DE" {
		t.Errorf("locale should have been reset from de-DE, still got %q", got)
	}
}

// =====================
// geo (geolocation) tests
// =====================

func TestGeo_OverridesGeolocation(t *testing.T) {
	page := navigateTo(t, "/")
	lat, lon := 48.8566, 2.3522 // Paris
	err := applyGeolocation(page, lat, lon)
	if err != nil {
		t.Fatalf("applyGeolocation failed: %v", err)
	}

	// Grant geolocation permission first
	err = (proto.BrowserGrantPermissions{
		Permissions: []proto.BrowserPermissionType{"geolocation"},
	}).Call(env.browser)
	if err != nil {
		t.Fatalf("grant permission failed: %v", err)
	}

	// Query position via JS
	result, err := page.Eval(`() => new Promise((resolve, reject) => {
		navigator.geolocation.getCurrentPosition(
			pos => resolve({lat: pos.coords.latitude, lon: pos.coords.longitude}),
			err => reject(err.message),
			{timeout: 5000}
		)
	})`)
	if err != nil {
		t.Fatalf("geolocation query failed: %v", err)
	}
	gotLat := result.Value.Get("lat").Num()
	gotLon := result.Value.Get("lon").Num()
	if gotLat != lat || gotLon != lon {
		t.Errorf("expected lat=%f lon=%f, got lat=%f lon=%f", lat, lon, gotLat, gotLon)
	}
}

func TestGeo_DifferentLocation(t *testing.T) {
	page := navigateTo(t, "/")
	lat, lon := 35.6762, 139.6503 // Tokyo
	err := applyGeolocation(page, lat, lon)
	if err != nil {
		t.Fatalf("applyGeolocation failed: %v", err)
	}

	err = (proto.BrowserGrantPermissions{
		Permissions: []proto.BrowserPermissionType{"geolocation"},
	}).Call(env.browser)
	if err != nil {
		t.Fatalf("grant permission failed: %v", err)
	}

	result, err := page.Eval(`() => new Promise((resolve, reject) => {
		navigator.geolocation.getCurrentPosition(
			pos => resolve({lat: pos.coords.latitude, lon: pos.coords.longitude}),
			err => reject(err.message),
			{timeout: 5000}
		)
	})`)
	if err != nil {
		t.Fatalf("geolocation query failed: %v", err)
	}
	gotLat := result.Value.Get("lat").Num()
	gotLon := result.Value.Get("lon").Num()
	if gotLat != lat || gotLon != lon {
		t.Errorf("expected lat=%f lon=%f, got lat=%f lon=%f", lat, lon, gotLat, gotLon)
	}
}

// =====================
// media emulation tests
// =====================

func TestMedia_PrefersColorScheme(t *testing.T) {
	page := navigateTo(t, "/")
	err := applyMedia(page, []*proto.EmulationMediaFeature{
		{Name: "prefers-color-scheme", Value: "dark"},
	})
	if err != nil {
		t.Fatalf("applyMedia failed: %v", err)
	}
	result, err := page.Eval(`() => window.matchMedia('(prefers-color-scheme: dark)').matches`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if result.Value.JSON("", "") != "true" {
		t.Error("expected prefers-color-scheme: dark to match")
	}
}

func TestMedia_PrefersReducedMotion(t *testing.T) {
	page := navigateTo(t, "/")
	err := applyMedia(page, []*proto.EmulationMediaFeature{
		{Name: "prefers-reduced-motion", Value: "reduce"},
	})
	if err != nil {
		t.Fatalf("applyMedia failed: %v", err)
	}
	result, err := page.Eval(`() => window.matchMedia('(prefers-reduced-motion: reduce)').matches`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if result.Value.JSON("", "") != "true" {
		t.Error("expected prefers-reduced-motion: reduce to match")
	}
}

func TestMedia_PrintMediaType(t *testing.T) {
	page := navigateTo(t, "/")
	err := applyMediaType(page, "print")
	if err != nil {
		t.Fatalf("applyMediaType failed: %v", err)
	}
	result, err := page.Eval(`() => window.matchMedia('print').matches`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if result.Value.JSON("", "") != "true" {
		t.Error("expected print media type to match")
	}
}

func TestMedia_MultipleFeatures(t *testing.T) {
	page := navigateTo(t, "/")
	err := applyMedia(page, []*proto.EmulationMediaFeature{
		{Name: "prefers-color-scheme", Value: "dark"},
		{Name: "prefers-reduced-motion", Value: "reduce"},
	})
	if err != nil {
		t.Fatalf("applyMedia failed: %v", err)
	}
	// Both should be active
	result1, err := page.Eval(`() => window.matchMedia('(prefers-color-scheme: dark)').matches`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	result2, err := page.Eval(`() => window.matchMedia('(prefers-reduced-motion: reduce)').matches`)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	if result1.Value.JSON("", "") != "true" {
		t.Error("expected prefers-color-scheme: dark to match")
	}
	if result2.Value.JSON("", "") != "true" {
		t.Error("expected prefers-reduced-motion: reduce to match")
	}
}

func TestInsecureFlag_WithSelfSignedCert(t *testing.T) {
	// Create HTTPS server with self-signed certificate
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html>
<html><head><title>Secure Test</title></head>
<body><h1>HTTPS Test Page</h1></body></html>`))
	})
	httpsServer := httptest.NewUnstartedServer(mux)
	// Suppress expected TLS handshake errors to keep test output clean
	httpsServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	httpsServer.StartTLS()
	defer httpsServer.Close()

	// Test 1: Browser WITHOUT --ignore-certificate-errors should fail
	t.Run("WithoutInsecureFlag", func(t *testing.T) {
		l := launcher.New().
			Set("no-sandbox").
			Set("disable-gpu").
			Set("single-process").
			Headless(true).
			Leakless(false)

		if bin := os.Getenv("ROD_CHROME_BIN"); bin != "" {
			l = l.Bin(bin)
		}

		u := l.MustLaunch()
		browser := rod.New().ControlURL(u).MustConnect()
		defer browser.MustClose()

		page := browser.MustPage("")
		defer page.MustClose()

		err := page.Navigate(httpsServer.URL)
		if err == nil {
			t.Fatal("expected ERR_CERT_AUTHORITY_INVALID error, but navigation succeeded")
		}
		if !strings.Contains(err.Error(), "ERR_CERT_AUTHORITY_INVALID") {
			t.Errorf("expected ERR_CERT_AUTHORITY_INVALID, got: %v", err)
		}
	})

	// Test 2: Browser WITH --ignore-certificate-errors should succeed
	t.Run("WithInsecureFlag", func(t *testing.T) {
		l := launcher.New().
			Set("no-sandbox").
			Set("disable-gpu").
			Set("single-process").
			Set("ignore-certificate-errors"). // This is what --insecure sets
			Headless(true).
			Leakless(false)

		if bin := os.Getenv("ROD_CHROME_BIN"); bin != "" {
			l = l.Bin(bin)
		}

		u := l.MustLaunch()
		browser := rod.New().ControlURL(u).MustConnect()
		defer browser.MustClose()

		// Try to navigate to HTTPS server with invalid cert
		page := browser.MustPage(httpsServer.URL)
		defer page.MustClose()

		page.MustWaitLoad()
		title := page.MustInfo().Title

		if title != "Secure Test" {
			t.Errorf("expected page to load successfully with title 'Secure Test', got %q", title)
		}
	})
}

// =====================
// cookie tests (RED)
// =====================

// Helper to clear all cookies before a cookie test
func clearCookies(t *testing.T, page *rod.Page) {
	t.Helper()
	err := proto.NetworkClearBrowserCookies{}.Call(page)
	if err != nil {
		t.Fatalf("failed to clear cookies: %v", err)
	}
}

func TestCookieSet_RoundTrip(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	// Set a cookie using --url approach (test server is on 127.0.0.1)
	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name:  "session_id",
		Value: "abc123",
		URL:   env.server.URL + "/",
	})

	// Read it back
	result, err := proto.NetworkGetCookies{Urls: []string{env.server.URL + "/"}}.Call(page)
	if err != nil {
		t.Fatalf("getCookies failed: %v", err)
	}

	found := false
	for _, c := range result.Cookies {
		if c.Name == "session_id" && c.Value == "abc123" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("cookie session_id=abc123 not found after setting it")
	}
}

func TestCookieSet_WithURL(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name:  "url_cookie",
		Value: "fromurl",
		URL:   env.server.URL + "/somepath",
	})

	result, err := proto.NetworkGetCookies{Urls: []string{env.server.URL + "/somepath"}}.Call(page)
	if err != nil {
		t.Fatalf("getCookies failed: %v", err)
	}

	found := false
	for _, c := range result.Cookies {
		if c.Name == "url_cookie" && c.Value == "fromurl" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("cookie url_cookie=fromurl not found after setting via URL")
	}
}

func TestCookieSet_SecureHTTPOnly(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name:     "secure_cookie",
		Value:    "secret",
		URL:      "https://secure.example.com/",
		Secure:   true,
		HTTPOnly: true,
	})

	result, err := proto.NetworkGetCookies{Urls: []string{"https://secure.example.com/"}}.Call(page)
	if err != nil {
		t.Fatalf("getCookies failed: %v", err)
	}

	for _, c := range result.Cookies {
		if c.Name == "secure_cookie" {
			if !c.Secure {
				t.Errorf("expected cookie to be secure")
			}
			if !c.HTTPOnly {
				t.Errorf("expected cookie to be httponly")
			}
			return
		}
	}
	t.Errorf("secure_cookie not found")
}

func TestParseCookieSetArgs_NoDomainOrURL(t *testing.T) {
	param, err := parseCookieSetArgs([]string{"name", "value"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No domain or URL set — cmdCookieSet will default to current page URL
	if param.Domain != "" {
		t.Errorf("domain = %q, want empty", param.Domain)
	}
	if param.URL != "" {
		t.Errorf("url = %q, want empty", param.URL)
	}
}

func TestCookieSet_DefaultsToCurrentPage(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	// Simulate what cmdCookieSet does: parse args with no --domain/--url,
	// then fill in the page URL
	param, err := parseCookieSetArgs([]string{"page_cookie", "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if param.Domain == "" && param.URL == "" {
		info, infoErr := page.Info()
		if infoErr != nil {
			t.Fatalf("failed to get page info: %v", infoErr)
		}
		param.URL = info.URL
	}

	err = proto.NetworkSetCookies{
		Cookies: []*proto.NetworkCookieParam{param},
	}.Call(page)
	if err != nil {
		t.Fatalf("setCookies failed: %v", err)
	}

	// Read it back
	result, infoErr := proto.NetworkGetCookies{Urls: []string{env.server.URL + "/"}}.Call(page)
	if infoErr != nil {
		t.Fatalf("getCookies failed: %v", infoErr)
	}
	found := false
	for _, c := range result.Cookies {
		if c.Name == "page_cookie" && c.Value == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("cookie page_cookie=hello not found — default to current page URL didn't work")
	}
}

func TestParseCookieSetArgs_WithDomain(t *testing.T) {
	param, err := parseCookieSetArgs([]string{"myname", "myvalue", "--domain", ".example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if param.Name != "myname" {
		t.Errorf("name = %q, want %q", param.Name, "myname")
	}
	if param.Value != "myvalue" {
		t.Errorf("value = %q, want %q", param.Value, "myvalue")
	}
	if param.Domain != ".example.com" {
		t.Errorf("domain = %q, want %q", param.Domain, ".example.com")
	}
}

func TestParseCookieSetArgs_WithURL(t *testing.T) {
	param, err := parseCookieSetArgs([]string{"n", "v", "--url", "https://example.com/path"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if param.URL != "https://example.com/path" {
		t.Errorf("url = %q, want %q", param.URL, "https://example.com/path")
	}
	if param.Domain != "" {
		t.Errorf("domain should be empty when --url used, got %q", param.Domain)
	}
}

func TestParseCookieSetArgs_AllFlags(t *testing.T) {
	param, err := parseCookieSetArgs([]string{
		"tok", "val",
		"--domain", ".example.com",
		"--path", "/api",
		"--secure",
		"--httponly",
		"--samesite", "Strict",
		"--expires", "1735689600",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if param.Path != "/api" {
		t.Errorf("path = %q, want %q", param.Path, "/api")
	}
	if !param.Secure {
		t.Error("expected secure=true")
	}
	if !param.HTTPOnly {
		t.Error("expected httponly=true")
	}
	if param.SameSite != proto.NetworkCookieSameSiteStrict {
		t.Errorf("samesite = %q, want Strict", param.SameSite)
	}
	if param.Expires == 0 {
		t.Error("expected expires to be set")
	}
}

func TestParseCookieSetArgs_TooFewArgs(t *testing.T) {
	_, err := parseCookieSetArgs([]string{"name"})
	if err == nil {
		t.Error("expected error for too few args")
	}
}

func TestCookieGet_ByName(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name:  "find_me",
		Value: "here",
		URL:   env.server.URL + "/",
	})

	cookies := getCookiesFromBrowser(page, []string{env.server.URL + "/"})
	val := ""
	for _, c := range cookies {
		if c.Name == "find_me" {
			val = c.Value
			break
		}
	}
	if val != "here" {
		t.Errorf("expected cookie value %q, got %q", "here", val)
	}
}

func TestCookieGet_All(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name: "c1", Value: "v1", URL: env.server.URL + "/",
	})
	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name: "c2", Value: "v2", URL: env.server.URL + "/",
	})

	cookies := getCookiesFromBrowser(page, []string{env.server.URL + "/"})
	names := map[string]bool{}
	for _, c := range cookies {
		names[c.Name] = true
	}
	if !names["c1"] || !names["c2"] {
		t.Errorf("expected both c1 and c2 cookies, got: %v", names)
	}
}

func TestCookieGet_JSON(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name: "json_test", Value: "jval", URL: env.server.URL + "/",
	})

	cookies := getCookiesFromBrowser(page, []string{env.server.URL + "/"})
	out := formatCookiesJSON(cookies)
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("JSON output is not valid: %v\noutput: %s", err, out)
	}
	if len(parsed) == 0 {
		t.Fatal("expected at least one cookie in JSON output")
	}
	found := false
	for _, c := range parsed {
		if c["name"] == "json_test" && c["value"] == "jval" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("cookie json_test not found in JSON output: %s", out)
	}
}

func TestCookieGet_FormatDefault(t *testing.T) {
	cookies := []*proto.NetworkCookie{
		{
			Name:     "sess",
			Value:    "abc",
			Domain:   ".example.com",
			Path:     "/",
			Secure:   true,
			HTTPOnly: true,
			Session:  false,
			Expires:  proto.TimeSinceEpoch(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix()),
		},
		{
			Name:    "theme",
			Value:   "dark",
			Domain:  ".example.com",
			Path:    "/",
			Session: true,
		},
	}
	out := formatCookiesDefault(cookies)
	if !strings.Contains(out, "sess") {
		t.Errorf("output should contain 'sess', got:\n%s", out)
	}
	if !strings.Contains(out, "secure") {
		t.Errorf("output should contain 'secure' flag, got:\n%s", out)
	}
	if !strings.Contains(out, "httponly") {
		t.Errorf("output should contain 'httponly' flag, got:\n%s", out)
	}
	if !strings.Contains(out, "session") {
		t.Errorf("output should contain 'session' for session cookie, got:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d:\n%s", len(lines), out)
	}
}

func TestCookieDelete_ByName(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name: "delete_me", Value: "gone", URL: env.server.URL + "/",
	})

	// Verify it exists
	cookies := getCookiesFromBrowser(page, []string{env.server.URL + "/"})
	found := false
	for _, c := range cookies {
		if c.Name == "delete_me" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("cookie should exist before deletion")
	}

	// Delete it
	deleteCookieFromBrowser(page, "delete_me", "", "", "")

	// Verify it's gone
	cookies = getCookiesFromBrowser(page, []string{env.server.URL + "/"})
	for _, c := range cookies {
		if c.Name == "delete_me" {
			t.Errorf("cookie delete_me should have been deleted")
		}
	}
}

func TestCookieDelete_WithDomain(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name: "scoped", Value: "v1", URL: env.server.URL + "/",
	})

	// Delete with domain filter
	deleteCookieFromBrowser(page, "scoped", "127.0.0.1", "", "")

	cookies := getCookiesFromBrowser(page, []string{env.server.URL + "/"})
	for _, c := range cookies {
		if c.Name == "scoped" {
			t.Errorf("cookie scoped should have been deleted")
		}
	}
}

func TestCookieClear(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name: "a", Value: "1", URL: env.server.URL + "/",
	})
	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name: "b", Value: "2", URL: env.server.URL + "/",
	})

	// Clear all
	err := proto.NetworkClearBrowserCookies{}.Call(page)
	if err != nil {
		t.Fatalf("clear cookies failed: %v", err)
	}

	cookies := getCookiesFromBrowser(page, []string{env.server.URL + "/"})
	if len(cookies) != 0 {
		t.Errorf("expected 0 cookies after clear, got %d", len(cookies))
	}
}

func TestCookieClear_WithDomain(t *testing.T) {
	page := navigateTo(t, "/")
	clearCookies(t, page)

	// Set cookies on two different domains
	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name: "keep", Value: "yes", URL: env.server.URL + "/",
	})
	setCookieOnBrowser(page, &proto.NetworkCookieParam{
		Name: "remove", Value: "bye", URL: "https://other.example.com/",
	})

	// Clear only other.example.com
	clearCookiesForDomain(page, "other.example.com")

	// Cookie on test server should still exist
	cookies := getCookiesFromBrowser(page, []string{env.server.URL + "/"})
	found := false
	for _, c := range cookies {
		if c.Name == "keep" {
			found = true
		}
	}
	if !found {
		t.Error("cookie 'keep' on test server should still exist after clearing other domain")
	}

	// Cookie on other.example.com should be gone
	cookies = getCookiesFromBrowser(page, []string{"https://other.example.com/"})
	for _, c := range cookies {
		if c.Name == "remove" {
			t.Error("cookie 'remove' on other.example.com should have been cleared")
		}
	}
}

func TestParseCookieGetArgs(t *testing.T) {
	name, urls, jsonOut := parseCookieGetArgs([]string{"mysess", "--domain", "example.com", "--json"})
	if name != "mysess" {
		t.Errorf("name = %q, want %q", name, "mysess")
	}
	if len(urls) != 1 || urls[0] != "https://example.com/" {
		t.Errorf("urls = %v, want [https://example.com/]", urls)
	}
	if !jsonOut {
		t.Error("expected json=true")
	}
}

func TestParseCookieGetArgs_URLFlag(t *testing.T) {
	name, urls, _ := parseCookieGetArgs([]string{"--url", "https://api.example.com/v1"})
	if name != "" {
		t.Errorf("name = %q, want empty", name)
	}
	if len(urls) != 1 || urls[0] != "https://api.example.com/v1" {
		t.Errorf("urls = %v, want [https://api.example.com/v1]", urls)
	}
}

func TestParseCookieGetArgs_NoArgs(t *testing.T) {
	name, urls, jsonOut := parseCookieGetArgs(nil)
	if name != "" {
		t.Errorf("name = %q, want empty", name)
	}
	if len(urls) != 0 {
		t.Errorf("urls = %v, want empty", urls)
	}
	if jsonOut {
		t.Error("expected json=false")
	}
}

func TestParseCookieDeleteArgs(t *testing.T) {
	name, domain, url, path, err := parseCookieDeleteArgs([]string{"sess", "--domain", ".example.com", "--path", "/app"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "sess" {
		t.Errorf("name = %q, want %q", name, "sess")
	}
	if domain != ".example.com" {
		t.Errorf("domain = %q, want %q", domain, ".example.com")
	}
	if url != "" {
		t.Errorf("url = %q, want empty", url)
	}
	if path != "/app" {
		t.Errorf("path = %q, want %q", path, "/app")
	}
}

func TestParseCookieDeleteArgs_NoName(t *testing.T) {
	_, _, _, _, err := parseCookieDeleteArgs(nil)
	if err == nil {
		t.Error("expected error when no name provided")
	}
}

func TestParseCookieDeleteArgs_URLFlag(t *testing.T) {
	name, domain, url, _, err := parseCookieDeleteArgs([]string{"sess", "--url", "https://example.com/app"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "sess" {
		t.Errorf("name = %q, want %q", name, "sess")
	}
	if domain != "" {
		t.Errorf("domain = %q, want empty", domain)
	}
	if url != "https://example.com/app" {
		t.Errorf("url = %q, want %q", url, "https://example.com/app")
	}
}

// Video recording tests
// =====================

// testStateDir overrides the state dir for test isolation
func withTestStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origStateDir := stateDirOverride
	stateDirOverride = dir
	t.Cleanup(func() { stateDirOverride = origStateDir })
	return dir
}

func TestStartVideo_SetsStateFlag(t *testing.T) {
	dir := withTestStateDir(t)

	// Write a fake state file (simulating a running browser)
	s := &State{DebugURL: "ws://fake", ChromePID: 99999}
	if err := saveState(s); err != nil {
		t.Fatal(err)
	}

	// Call startVideo
	if err := startVideo(); err != nil {
		t.Fatalf("startVideo failed: %v", err)
	}

	// State should now have VideoRecording=true and a VideoDir
	s2, err := loadState()
	if err != nil {
		t.Fatal(err)
	}
	if !s2.VideoRecording {
		t.Error("expected VideoRecording=true")
	}
	if s2.VideoDir == "" {
		t.Error("expected VideoDir to be set")
	}

	// VideoDir should exist on disk
	info, err := os.Stat(s2.VideoDir)
	if err != nil {
		t.Fatalf("VideoDir does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Error("VideoDir is not a directory")
	}

	// VideoDir should be under our state dir
	if !strings.HasPrefix(s2.VideoDir, dir) {
		t.Errorf("VideoDir %q should be under state dir %q", s2.VideoDir, dir)
	}
}

func TestStartVideo_ErrorsIfAlreadyRecording(t *testing.T) {
	withTestStateDir(t)

	s := &State{DebugURL: "ws://fake", ChromePID: 99999, VideoRecording: true, VideoDir: "/tmp/fake"}
	saveState(s)

	err := startVideo()
	if err == nil {
		t.Error("expected error when already recording")
	}
}

func TestVideoCapture_RecordsFramesDuringPageUse(t *testing.T) {
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")

	// Navigate to a page with animation (generates continuous frames)
	page := navigateTo(t, "/animated")

	// Start video capture on this page
	stop := startVideoCapture(page, framesDir)

	// Give screencast time to emit some frames
	time.Sleep(1 * time.Second)

	// Stop capture and get frame count
	n := stop()

	if n == 0 {
		t.Fatal("expected at least 1 frame captured, got 0")
	}

	// Check frames exist on disk
	entries, err := os.ReadDir(framesDir)
	if err != nil {
		t.Fatal(err)
	}

	jpegCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".jpeg" {
			jpegCount++
		}
	}
	if jpegCount == 0 {
		t.Fatal("no JPEG files found in frames dir")
	}
	if jpegCount != n {
		t.Errorf("frame count mismatch: stop() returned %d but found %d files", n, jpegCount)
	}

	// Check metadata file exists
	metaPath := filepath.Join(framesDir, "meta.jsonl")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("meta.jsonl not found: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(metaData)), "\n")
	if len(lines) != n {
		t.Errorf("meta.jsonl has %d lines, expected %d", len(lines), n)
	}

	// First frame should be valid JPEG
	firstFrame, err := os.ReadFile(filepath.Join(framesDir, "frame_000000.jpeg"))
	if err != nil {
		t.Fatalf("could not read first frame: %v", err)
	}
	if len(firstFrame) < 3 || firstFrame[0] != 0xFF || firstFrame[1] != 0xD8 {
		t.Error("first frame is not a valid JPEG (missing magic bytes)")
	}
}

func TestVideoCapture_AccumulatesAcrossCalls(t *testing.T) {
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")

	page := navigateTo(t, "/animated")

	// First capture session
	stop1 := startVideoCapture(page, framesDir)
	time.Sleep(500 * time.Millisecond)
	n1 := stop1()

	// Second capture session (should continue numbering)
	stop2 := startVideoCapture(page, framesDir)
	time.Sleep(500 * time.Millisecond)
	n2 := stop2()

	if n1 == 0 || n2 == 0 {
		t.Fatalf("expected frames from both sessions, got %d and %d", n1, n2)
	}

	// Total files should be n1 + n2
	entries, _ := os.ReadDir(framesDir)
	jpegCount := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".jpeg" {
			jpegCount++
		}
	}
	if jpegCount != n1+n2 {
		t.Errorf("expected %d total frames, got %d", n1+n2, jpegCount)
	}
}

func TestStopVideo_ClearsStateAndReturnsFrameCount(t *testing.T) {
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")
	os.MkdirAll(framesDir, 0755)

	// Simulate some captured frames + metadata
	for i := 0; i < 5; i++ {
		// Write minimal JPEG files (just magic bytes for test)
		os.WriteFile(filepath.Join(framesDir, fmt.Sprintf("frame_%06d.jpeg", i)), []byte{0xFF, 0xD8, 0xFF}, 0644)
	}
	metaLines := ""
	for i := 0; i < 5; i++ {
		metaLines += fmt.Sprintf(`{"idx":%d,"ts":%f}`+"\n", i, float64(1000+i)*0.016)
	}
	os.WriteFile(filepath.Join(framesDir, "meta.jsonl"), []byte(metaLines), 0644)

	s := &State{DebugURL: "ws://fake", ChromePID: 99999, VideoRecording: true, VideoDir: framesDir}
	saveState(s)

	result, err := stopVideo("")
	if err != nil {
		t.Fatalf("stopVideo failed: %v", err)
	}

	if result.FrameCount != 5 {
		t.Errorf("expected 5 frames, got %d", result.FrameCount)
	}

	// State should be cleared
	s2, err := loadState()
	if err != nil {
		t.Fatal(err)
	}
	if s2.VideoRecording {
		t.Error("expected VideoRecording=false after stop")
	}
	if s2.VideoDir != "" {
		t.Error("expected VideoDir to be cleared after stop")
	}
}

func TestStopVideo_ErrorsIfNotRecording(t *testing.T) {
	withTestStateDir(t)

	s := &State{DebugURL: "ws://fake", ChromePID: 99999}
	saveState(s)

	_, err := stopVideo("")
	if err == nil {
		t.Error("expected error when not recording")
	}
}

func TestAssembleVideo_ProducesMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}

	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")

	// Capture real frames from an animated page
	page := navigateTo(t, "/animated")
	stop := startVideoCapture(page, framesDir)
	time.Sleep(1 * time.Second)
	n := stop()

	if n < 2 {
		t.Fatalf("need at least 2 frames for video, got %d", n)
	}

	outputFile := filepath.Join(dir, "test-output.mp4")
	result, err := assembleVideo(framesDir, outputFile)
	if err != nil {
		t.Fatalf("assembleVideo failed: %v", err)
	}

	info, err := os.Stat(result)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("output file is empty")
	}

	// Verify it's a real MP4 (starts with ftyp box or moov)
	header := make([]byte, 12)
	f, _ := os.Open(result)
	f.Read(header)
	f.Close()
	// MP4 files have "ftyp" at offset 4
	if string(header[4:8]) != "ftyp" {
		t.Errorf("output doesn't look like MP4, header: %x", header[:12])
	}
}

func TestVideoCapture_WithPageIntegration(t *testing.T) {
	// Test that withPageVideoCapture starts/stops screencast when recording is on
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")
	os.MkdirAll(framesDir, 0755)

	page := navigateTo(t, "/animated")

	// Simulate: recording is active
	s := &State{
		DebugURL:       "ws://fake",
		ChromePID:      99999,
		VideoRecording: true,
		VideoDir:       framesDir,
	}
	saveState(s)

	// Call the integration hook — same thing withPage() calls
	cleanup := maybeStartVideoCapture(page)

	time.Sleep(1 * time.Second)

	// Call cleanup (same as what runs via defer in main)
	cleanup()

	// Frames should have been captured
	frameCount := countFrames(framesDir)
	if frameCount == 0 {
		t.Fatal("expected frames to be captured via maybeStartVideoCapture")
	}
}

func TestStopVideo_ProducesMP4WhenFfmpegAvailable(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}

	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")

	// Capture real frames from animated page
	page := navigateTo(t, "/animated")
	stop := startVideoCapture(page, framesDir)
	time.Sleep(1 * time.Second)
	stop()

	// Set up state as if start-video had run
	s := &State{DebugURL: "ws://fake", ChromePID: 99999, VideoRecording: true, VideoDir: framesDir}
	saveState(s)

	outputFile := filepath.Join(dir, "result.mp4")
	result, err := stopVideo(outputFile)
	if err != nil {
		t.Fatalf("stopVideo failed: %v", err)
	}

	if result.OutputFile == "" {
		t.Error("expected OutputFile to be set")
	}
	if result.FrameCount == 0 {
		t.Error("expected non-zero frame count")
	}

	// MP4 file should exist
	info, err := os.Stat(result.OutputFile)
	if err != nil {
		t.Fatalf("MP4 file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("MP4 file is empty")
	}

	// Frames dir should be cleaned up
	if _, err := os.Stat(framesDir); !os.IsNotExist(err) {
		t.Error("expected frames dir to be removed after stop-video")
	}
}

// =====================
// GIF recording tests
// =====================

func TestAssembleGIF_ProducesValidGIF(t *testing.T) {
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")

	// Capture real frames from animated page
	page := navigateTo(t, "/animated")
	stop := startVideoCapture(page, framesDir)
	time.Sleep(1 * time.Second)
	n := stop()

	if n < 2 {
		t.Fatalf("need at least 2 frames, got %d", n)
	}

	outputFile := filepath.Join(dir, "test-output.gif")
	result, err := assembleGIF(framesDir, outputFile)
	if err != nil {
		t.Fatalf("assembleGIF failed: %v", err)
	}

	// File should exist and be non-empty
	info, err := os.Stat(result.OutputFile)
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("output file is empty")
	}

	// Should be a valid GIF (starts with GIF89a or GIF87a)
	header := make([]byte, 6)
	f, _ := os.Open(result.OutputFile)
	f.Read(header)
	f.Close()
	if string(header[:3]) != "GIF" {
		t.Errorf("not a GIF file, header: %q", string(header))
	}

	// Should have captured some frames
	if result.InputFrames == 0 {
		t.Error("expected non-zero InputFrames")
	}
	if result.UniqueFrames == 0 {
		t.Error("expected non-zero UniqueFrames")
	}
}

func TestAssembleGIF_DeduplicatesIdenticalFrames(t *testing.T) {
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")
	os.MkdirAll(framesDir, 0755)

	// Write identical JPEG frames to simulate duplicate screencast output
	// Use a real JPEG from a page capture for realistic data
	page := navigateTo(t, "/")
	stop := startVideoCapture(page, framesDir)
	time.Sleep(200 * time.Millisecond)
	stop()

	// Read whatever frame we got and duplicate it
	firstFrame, err := os.ReadFile(filepath.Join(framesDir, "frame_000000.jpeg"))
	if err != nil {
		t.Fatalf("no frame captured: %v", err)
	}

	// Clear and write 10 identical frames + metadata
	os.RemoveAll(framesDir)
	os.MkdirAll(framesDir, 0755)
	metaFile, _ := os.Create(filepath.Join(framesDir, "meta.jsonl"))
	for i := 0; i < 10; i++ {
		os.WriteFile(filepath.Join(framesDir, fmt.Sprintf("frame_%06d.jpeg", i)), firstFrame, 0644)
		fmt.Fprintf(metaFile, `{"idx":%d,"ts":%.6f}`+"\n", i, float64(1000)+float64(i)*0.033)
	}
	metaFile.Close()

	outputFile := filepath.Join(dir, "dedup-test.gif")
	result, err := assembleGIF(framesDir, outputFile)
	if err != nil {
		t.Fatalf("assembleGIF failed: %v", err)
	}

	t.Logf("Input: %d frames, Unique: %d frames", result.InputFrames, result.UniqueFrames)

	// All 10 frames are identical, so should deduplicate to 1 unique frame
	if result.UniqueFrames != 1 {
		t.Errorf("expected 1 unique frame from 10 identical inputs, got %d", result.UniqueFrames)
	}
	if result.InputFrames != 10 {
		t.Errorf("expected 10 input frames, got %d", result.InputFrames)
	}
}

func TestAssembleGIF_DecodableWithStdlib(t *testing.T) {
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")

	page := navigateTo(t, "/animated")
	stop := startVideoCapture(page, framesDir)
	time.Sleep(1 * time.Second)
	stop()

	outputFile := filepath.Join(dir, "decode-test.gif")
	_, err := assembleGIF(framesDir, outputFile)
	if err != nil {
		t.Fatalf("assembleGIF failed: %v", err)
	}

	// Decode the GIF with stdlib to verify it's valid
	f, err := os.Open(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	g, err := gif.DecodeAll(f)
	if err != nil {
		t.Fatalf("gif.DecodeAll failed: %v", err)
	}

	if len(g.Image) == 0 {
		t.Error("GIF has no frames")
	}
	if len(g.Image) != len(g.Delay) {
		t.Errorf("frame count (%d) != delay count (%d)", len(g.Image), len(g.Delay))
	}
	// LoopCount 0 means loop forever
	if g.LoopCount != 0 {
		t.Errorf("expected LoopCount 0 (loop forever), got %d", g.LoopCount)
	}

	// Check dimensions match our viewport
	bounds := g.Image[0].Bounds()
	if bounds.Dx() == 0 || bounds.Dy() == 0 {
		t.Errorf("first frame has zero dimensions: %v", bounds)
	}

	// All delays should be positive
	for i, d := range g.Delay {
		if d <= 0 {
			t.Errorf("frame %d has non-positive delay: %d", i, d)
		}
	}
}

func TestSleep_CapturesVideoFramesWhenRecording(t *testing.T) {
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")
	os.MkdirAll(framesDir, 0755)

	// Navigate to an animated page (simulates a page being open)
	page := navigateTo(t, "/animated")

	// Set up state: recording is active
	s := &State{
		DebugURL:       "ws://fake",
		ChromePID:      99999,
		VideoRecording: true,
		VideoDir:       framesDir,
	}
	saveState(s)

	// maybeStartVideoCapture should start screencast when recording is on
	cleanup := maybeStartVideoCapture(page)
	time.Sleep(2 * time.Second)
	cleanup()

	// Frames should have been captured during the sleep
	frameCount := countFrames(framesDir)
	if frameCount == 0 {
		t.Fatal("expected frames to be captured during sleep while recording, got 0")
	}
	t.Logf("captured %d frames during 2s sleep", frameCount)
}

func TestStopVideo_MP4FallsBackToGIFWithoutFfmpeg(t *testing.T) {
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")

	page := navigateTo(t, "/animated")
	stop := startVideoCapture(page, framesDir)
	time.Sleep(1 * time.Second)
	stop()

	s := &State{DebugURL: "ws://fake", ChromePID: 99999, VideoRecording: true, VideoDir: framesDir}
	saveState(s)

	// Request .mp4 but with a bogus PATH so ffmpeg won't be found
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	outputFile := filepath.Join(dir, "result.mp4")
	result, err := stopVideo(outputFile)
	if err != nil {
		t.Fatalf("stopVideo failed: %v", err)
	}

	// Should have fallen back to GIF
	if result.OutputFile == "" {
		t.Fatal("expected OutputFile to be set (GIF fallback)")
	}
	if !strings.HasSuffix(result.OutputFile, ".gif") {
		t.Errorf("expected .gif fallback, got %q", result.OutputFile)
	}

	// Verify it's actually a valid GIF
	header := make([]byte, 6)
	f, _ := os.Open(result.OutputFile)
	f.Read(header)
	f.Close()
	if string(header[:3]) != "GIF" {
		t.Errorf("fallback file is not a GIF, header: %q", string(header))
	}
}

func TestStopVideo_DetectsGIFExtension(t *testing.T) {
	dir := withTestStateDir(t)
	framesDir := filepath.Join(dir, "video-frames")

	page := navigateTo(t, "/animated")
	stop := startVideoCapture(page, framesDir)
	time.Sleep(1 * time.Second)
	stop()

	s := &State{DebugURL: "ws://fake", ChromePID: 99999, VideoRecording: true, VideoDir: framesDir}
	saveState(s)

	outputFile := filepath.Join(dir, "result.gif")
	result, err := stopVideo(outputFile)
	if err != nil {
		t.Fatalf("stopVideo failed: %v", err)
	}

	if result.OutputFile == "" {
		t.Fatal("expected OutputFile to be set")
	}

	// Should be a valid GIF
	header := make([]byte, 6)
	f, _ := os.Open(result.OutputFile)
	f.Read(header)
	f.Close()
	if string(header[:3]) != "GIF" {
		t.Errorf("expected GIF file for .gif extension, got header: %q", string(header))
	}
}
