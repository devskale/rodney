# Jodney: Chrome automation from the command line

[![PyPI](https://img.shields.io/pypi/v/jodney.svg)](https://pypi.org/project/jodney/)
[![Changelog](https://img.shields.io/github/v/release/devskale/jodney?include_prereleases&label=changelog)](https://github.com/devskale/jodney/releases)
[![Tests](https://github.com/devskale/jodney/actions/workflows/test.yml/badge.svg)](https://github.com/devskale/jodney/actions/workflows/test.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](https://github.com/devskale/jodney/blob/main/LICENSE)

A Go CLI tool that drives a persistent headless Chrome instance using the [rod](https://github.com/go-rod/rod) browser automation library. Each command connects to the same long-running Chrome process, making it easy to script multi-step browser interactions from shell scripts or interactive use.

## Architecture

```
jodney start          →  launches Chrome (headless, persists after CLI exits)
                          saves WebSocket debug URL to ~/.jodney/state.json

jodney connect H:P    →  connects to an existing Chrome on a remote debug port
                          saves WebSocket debug URL to ~/.jodney/state.json

jodney open URL       →  connects to running Chrome via WebSocket
                          navigates the active tab, disconnects

jodney js EXPR        →  connects, evaluates JS, prints result, disconnects

jodney stop           →  connects and shuts down Chrome, cleans up state
```

Each CLI invocation is a short-lived process. Chrome runs independently and tabs persist between commands.

## Building

```bash
go build -o jodney .
```

Requires:
- Go 1.21+
- Google Chrome or Chromium installed (or set `ROD_CHROME_BIN=/path/to/chrome`)

## Usage

### Start/stop the browser

```bash
jodney start              # Launch headless Chrome
jodney start --show       # Launch with visible browser window
jodney start --insecure   # Launch with TLS errors ignored (-k shorthand)
jodney connect host:9222  # Connect to existing Chrome on remote debug port
jodney status             # Show browser info and active page
jodney stop               # Shut down Chrome
```

### Navigate

```bash
jodney open https://example.com    # Navigate to URL
jodney open example.com            # http:// prefix added automatically
jodney back                        # Go back
jodney forward                     # Go forward
jodney reload                      # Reload page
jodney reload --hard               # Reload bypassing cache
jodney clear-cache                 # Clear the browser cache
```

### Extract information

```bash
jodney url                    # Print current URL
jodney title                  # Print page title
jodney text "h1"              # Print text content of element
jodney html "div.content"     # Print outer HTML of element
jodney html                   # Print full page HTML
jodney attr "a#link" href     # Print attribute value
jodney pdf output.pdf         # Save page as PDF
```

### Run JavaScript

```bash
jodney js document.title                        # Evaluate expression
jodney js "1 + 2"                               # Math
jodney js 'document.querySelector("h1").textContent'  # DOM queries
jodney js '[1,2,3].map(x => x * 2)'            # Returns pretty-printed JSON
jodney js 'document.querySelectorAll("a").length'     # Count elements
```

The expression is automatically wrapped in `() => { return (expr); }`.

### Interact with elements

```bash
jodney click "button#submit"       # Click element
jodney input "#search" "query"     # Type into input field
jodney clear "#search"             # Clear input field
jodney file "#upload" photo.png    # Set file on a file input
jodney file "#upload" -            # Set file from stdin
jodney download "a.pdf-link"       # Download href/src target to file
jodney download "a.pdf-link" -     # Download to stdout
jodney select "#dropdown" "value"  # Select dropdown by value
jodney submit "form#login"         # Submit a form
jodney hover ".menu-item"          # Hover over element
jodney focus "#email"              # Focus element
```

### Wait for conditions

```bash
jodney wait ".loaded"       # Wait for element to appear and be visible
jodney waitload             # Wait for page load event
jodney waitstable           # Wait for DOM to stop changing
jodney waitidle             # Wait for network to be idle
jodney sleep 2.5            # Sleep for N seconds
```

### Screenshots

```bash
jodney screenshot                         # Save as screenshot.png
jodney screenshot page.png                # Save to specific file
jodney screenshot -w 1280 -h 720 out.png  # Set viewport width/height
jodney screenshot-el ".chart" chart.png   # Screenshot specific element
```

### Manage tabs

```bash
jodney pages                    # List all tabs (* marks active)
jodney newpage https://...      # Open URL in new tab
jodney page 1                   # Switch to tab by index
jodney closepage 1              # Close tab by index
jodney closepage                # Close active tab
```

### Query elements

```bash
jodney exists ".loading"    # Exit 0 if exists, exit 1 if not
jodney count "li.item"      # Print number of matching elements
jodney visible "#modal"     # Exit 0 if visible, exit 1 if not
jodney assert 'document.title' 'Home'  # Exit 0 if equal, exit 1 if not
jodney assert 'document.querySelector("h1") !== null'  # Exit 0 if truthy
```

### Accessibility testing

```bash
jodney ax-tree                           # Dump full accessibility tree
jodney ax-tree --depth 3                 # Limit tree depth
jodney ax-tree --json                    # Output as JSON

jodney ax-find --role button             # Find all buttons
jodney ax-find --name "Submit"           # Find by accessible name
jodney ax-find --role link --name "Home" # Combine filters
jodney ax-find --role button --json      # Output as JSON

jodney ax-node "#submit-btn"             # Inspect element's a11y properties
jodney ax-node "h1" --json               # Output as JSON
```

These commands use Chrome's [Accessibility CDP domain](https://chromedevtools.github.io/devtools-protocol/tot/Accessibility/) to expose what assistive technologies see. `ax-tree` uses `getFullAXTree`, `ax-find` uses `queryAXTree`, and `ax-node` uses `getPartialAXTree`.

```bash
# CI check: verify all buttons have accessible names
jodney ax-find --role button --json | python3 -c "
import json, sys
buttons = json.load(sys.stdin)
unnamed = [b for b in buttons if not b.get('name', {}).get('value')]
if unnamed:
    print(f'FAIL: {len(unnamed)} button(s) missing accessible name')
    sys.exit(1)
print(f'PASS: all {len(buttons)} buttons have accessible names')
"
```

### Directory-scoped sessions

By default, Jodney stores state globally in `~/.jodney/`. You can instead create a session scoped to the current directory with `--local`:

```bash
jodney start --local          # State stored in ./.jodney/state.json
                              # Chrome data in ./.jodney/chrome-data/
jodney open https://example.com   # Auto-detects local session
jodney stop                       # Cleans up local session
```

This is useful when you want isolated browser sessions per project — each directory gets its own Chrome instance, cookies, and state.

**Auto-detection:** When neither `--local` nor `--global` is specified, Jodney checks for `./.jodney/state.json` in the current directory. If found, it uses the local session; otherwise it falls back to the global `~/.jodney/` session.

```bash
# Force global even when a local session exists
jodney --global open https://example.com

# Force local (errors if no local session)
jodney --local status
```

The `--local` and `--global` flags can appear anywhere in the command:

```bash
jodney --local start
jodney start --local          # Same effect
jodney open --global https://example.com
```

Add `.jodney/` to your `.gitignore` to keep session state out of version control.

### Shell scripting examples

```bash
# Wait for page to load and extract data
jodney start
jodney open https://example.com
jodney waitstable
title=$(jodney title)
echo "Page: $title"

# Conditional logic based on element presence
if jodney exists ".error-message"; then
    jodney text ".error-message"
fi

# Loop through pages
for url in page1 page2 page3; do
    jodney open "https://example.com/$url"
    jodney waitstable
    jodney screenshot "${url}.png"
done

jodney stop
```

## Exit codes

Jodney uses distinct exit codes to separate check failures from errors:

| Exit code | Meaning |
|---|---|
| `0` | Success |
| `1` | Check failed — the command ran successfully but the condition/assertion was not met |
| `2` | Error — something went wrong (bad arguments, no browser session, timeout, etc.) |

This makes it easy to distinguish between "the assertion is false" and "the command couldn't run" in scripts and CI pipelines.

## Using Jodney for checks

Several commands return **exit code 1** when a condition is not met, making them useful as assertions in shell scripts and CI pipelines. All of these print their result to stdout and exit cleanly — no error message is written to stderr.

### `exists` — check if an element exists in the DOM

```bash
jodney exists "h1"
# Prints "true", exits 0

jodney exists ".nonexistent"
# Prints "false", exits 1
```

### `visible` — check if an element is visible

```bash
jodney visible "#modal"
# Prints "true" and exits 0 if the element exists and is visible

jodney visible "#hidden-div"
# Prints "false" and exits 1 if the element is hidden or doesn't exist
```

### `ax-find` — check for accessibility nodes

```bash
jodney ax-find --role button --name "Submit"
# Prints the matching node(s), exits 0

jodney ax-find --role banner --name "Nonexistent"
# Prints "No matching nodes" to stderr, exits 1
```

### `assert` — assert a JavaScript expression

With one argument, checks that the expression is truthy. With two arguments, checks that the expression's value equals the expected string. Use `--message` / `-m` to set a custom failure message.

```bash
# Truthy mode — check that expression evaluates to a truthy value
jodney assert 'document.querySelector(".logged-in") !== null'
# Prints "pass", exits 0

jodney assert 'document.querySelector(".nonexistent")'
# Prints "fail: got null", exits 1

# Equality mode — check that expression result matches expected value
jodney assert 'document.title' 'Dashboard'
# Prints "pass" if title is "Dashboard", exits 0

jodney assert 'document.querySelectorAll(".item").length' '3'
# Prints "pass" if there are exactly 3 items, exits 0

jodney assert 'document.title' 'Wrong Title'
# Prints 'fail: got "Dashboard", expected "Wrong Title"', exits 1
```

The expression is evaluated the same way as `jodney js` — the result is converted to its string representation before comparison. This means `jodney assert 'document.title' 'Dashboard'` compares the unquoted string, and `jodney assert '1 + 2' '3'` compares the number as a string.

Use `--message` (or `-m`) to add a human-readable description to the failure output:

```bash
jodney assert 'document.querySelector(".logged-in")' -m "User should be logged in"
# On failure: "fail: User should be logged in (got null)"

jodney assert 'document.title' 'Dashboard' --message "Wrong page loaded"
# On failure: 'fail: Wrong page loaded (got "Home", expected "Dashboard")'
```

### Combining checks in a shell script

You can chain these together in a single script to run multiple assertions. Because check failures use exit code 1 while real errors use exit code 2, you can use `set -e` to abort on errors while handling check failures explicitly:

```bash
#!/bin/bash
set -euo pipefail

FAIL=0

check() {
    if ! "$@"; then
        echo "FAIL: $*"
        FAIL=1
    fi
}

jodney start
jodney open "https://example.com"
jodney waitstable

# Assert elements exist
check jodney exists "h1"
check jodney exists "nav"
check jodney exists "footer"

# Assert key elements are visible
check jodney visible "h1"
check jodney visible "#main-content"

# Assert JS expressions
check jodney assert 'document.title' 'Example Domain'
check jodney assert 'document.querySelectorAll("p").length' '2'
check jodney assert 'document.querySelector("h1") !== null'

# Assert accessibility requirements
check jodney ax-find --role navigation
check jodney ax-find --role heading --name "Example Domain"

jodney stop

if [ "$FAIL" -ne 0 ]; then
    echo "Some checks failed"
    exit 1
fi
echo "All checks passed"
```

This pattern is useful in CI — run Jodney as a post-deploy check, an accessibility audit, or a smoke test against a staging environment. Because exit code 2 signals an actual error (e.g. Chrome didn't start), `set -e` will abort the script immediately if something is broken rather than reporting a misleading test failure.

## Configuration

| Environment Variable | Default | Description |
|---|---|---|
| `JODNEY_HOME` | `~/.jodney` | Data directory for state and Chrome profile |
| `ROD_CHROME_BIN` | `/usr/bin/google-chrome` | Path to Chrome/Chromium binary |
| `ROD_TIMEOUT` | `30` | Default timeout in seconds for element queries |
| `HTTPS_PROXY` / `HTTP_PROXY` | (none) | Authenticated proxy auto-detected on start |

Global state is stored in `~/.jodney/state.json` with Chrome user data in `~/.jodney/chrome-data/`. When using `--local`, state is stored in `./.jodney/state.json` and `./.jodney/chrome-data/` in the current directory instead. Set `JODNEY_HOME` to override the default global directory.

## Proxy support

In environments with authenticated HTTP proxies (e.g., `HTTPS_PROXY=http://user:pass@host:port`), `jodney start` automatically:

1. Detects the proxy credentials from environment variables
2. Launches a local forwarding proxy that injects `Proxy-Authorization` headers into CONNECT requests
3. Configures Chrome to use the local proxy

This is necessary because Chrome cannot natively authenticate to proxies during HTTPS tunnel (CONNECT) establishment. The local proxy runs as a background process and is automatically cleaned up by `jodney stop`.

See [claude-code-chrome-proxy.md](claude-code-chrome-proxy.md) for detailed technical notes.

## How it works

The tool uses the [rod](https://github.com/go-rod/rod) Go library which communicates with Chrome via the DevTools Protocol (CDP) over WebSocket. Key implementation details:

- **`start`** uses rod's `launcher` package to start Chrome with `Leakless(false)` so Chrome survives after the CLI exits
- **Proxy auth** handled via a local forwarding proxy that bridges Chrome to authenticated upstream proxies
- **State persistence** via a JSON file containing the WebSocket debug URL and Chrome PID
- **Each command** creates a new rod `Browser` connection to the same Chrome instance, executes the operation, and disconnects
- **Element queries** use rod's built-in auto-wait with a configurable timeout (default 30s)
- **JS evaluation** wraps user expressions in arrow functions as required by rod's `Eval`
- **Accessibility commands** call CDP's Accessibility domain directly via rod's `proto` package (`getFullAXTree`, `queryAXTree`, `getPartialAXTree`)

## Dependencies

- [github.com/go-rod/rod](https://github.com/go-rod/rod) v0.116.2 - Chrome DevTools Protocol automation

## Commands reference

| Command | Arguments | Description |
|---|---|---|
| `start` | `[--show] [--insecure\|-k]` | Launch Chrome (headless by default, `--show` for visible) |
| `connect` | `<host:port>` | Connect to existing Chrome on remote debug port |
| `stop` | | Shut down Chrome |
| `status` | | Show browser status |
| `open` | `<url>` | Navigate to URL |
| `back` | | Go back in history |
| `forward` | | Go forward in history |
| `reload` | `[--hard]` | Reload page (`--hard` bypasses cache) |
| `clear-cache` | | Clear the browser cache |
| `url` | | Print current URL |
| `title` | | Print page title |
| `html` | `[selector]` | Print HTML (page or element) |
| `text` | `<selector>` | Print element text content |
| `attr` | `<selector> <name>` | Print attribute value |
| `pdf` | `[file]` | Save page as PDF |
| `js` | `<expression>` | Evaluate JavaScript |
| `click` | `<selector>` | Click element |
| `input` | `<selector> <text>` | Type into input |
| `clear` | `<selector>` | Clear input |
| `file` | `<selector> <path\|->` | Set file on a file input (`-` for stdin) |
| `download` | `<selector> [file\|-]` | Download href/src target (`-` for stdout) |
| `select` | `<selector> <value>` | Select dropdown value |
| `submit` | `<selector>` | Submit form |
| `hover` | `<selector>` | Hover over element |
| `focus` | `<selector>` | Focus element |
| `wait` | `<selector>` | Wait for element to appear |
| `waitload` | | Wait for page load |
| `waitstable` | | Wait for DOM stability |
| `waitidle` | | Wait for network idle |
| `sleep` | `<seconds>` | Sleep N seconds |
| `screenshot` | `[-w N] [-h N] [file]` | Page screenshot (optional viewport size) |
| `screenshot-el` | `<selector> [file]` | Element screenshot |
| `pages` | | List tabs |
| `page` | `<index>` | Switch tab |
| `newpage` | `[url]` | Open new tab |
| `closepage` | `[index]` | Close tab |
| `exists` | `<selector>` | Check element exists (exit 1 if not) |
| `count` | `<selector>` | Count matching elements |
| `visible` | `<selector>` | Check element visible (exit 1 if not) |
| `assert` | `<expr> [expected] [-m msg]` | Assert JS expression is truthy or equals expected (exit 1 if not) |
| `ax-tree` | `[--depth N] [--json]` | Dump accessibility tree |
| `ax-find` | `[--name N] [--role R] [--json]` | Find accessible nodes |
| `ax-node` | `<selector> [--json]` | Show element accessibility info |

### Global flags

| Flag | Description |
|---|---|
| `--local` | Use directory-scoped session (`./.jodney/`) |
| `--global` | Use global session (`~/.jodney/`) |
| `--version` | Print version and exit |
| `--help`, `-h`, `help` | Show help message |
