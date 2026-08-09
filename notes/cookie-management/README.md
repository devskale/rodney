# Cookie Management Design

## Summary

Add commands for setting, reading, and deleting browser cookies. This uses Chrome DevTools Protocol's `Network.setCookies`, `Network.getCookies`, and `Network.deleteCookies` — all already available via rod's `proto` package.

## Proposed Commands

### `rodney cookie-set` — Set a cookie

```
rodney cookie-set <name> <value> --domain <domain> [options]
```

**Options:**
- `--domain <domain>` — Cookie domain (e.g. `.example.com`). Required unless `--url` is provided.
- `--url <url>` — Alternative to `--domain`: associate cookie with this URL (CDP infers domain, path, and scheme from it)
- `--path <path>` — Cookie path (default: `/`)
- `--secure` — Mark as secure (HTTPS only)
- `--httponly` — Mark as HTTP-only (not accessible to JavaScript)
- `--samesite <value>` — `Strict`, `Lax`, or `None`
- `--expires <seconds>` — Expiry as Unix timestamp. Omit for session cookie.

Either `--domain` or `--url` must be provided. When in doubt, use `--domain` — it's explicit about exactly what gets set.

**Examples:**
```bash
# Set a cookie on a specific domain
rodney cookie-set session_id abc123 --domain .example.com

# Secure cookie with expiry
rodney cookie-set token xyz --domain .example.com --secure --httponly --expires 1735689600

# SameSite policy
rodney cookie-set prefs val --domain .example.com --samesite Strict

# Using --url (CDP infers domain/path/scheme from the URL)
rodney cookie-set theme dark --url https://example.com/app
```

**Implementation:** Calls `proto.NetworkSetCookies` with a single `NetworkCookieParam`. Either `--domain` or `--url` must be provided — the command fails with exit code 2 if neither is given.

### `rodney cookie-get` — Read cookies

```
rodney cookie-get [name] [options]
```

**Options:**
- `--domain <domain>` — Get cookies for a specific domain. Constructs an `https://<domain>/` URL for the CDP call. Can be repeated.
- `--url <url>` — Alternative to `--domain`: get cookies that would be sent to this URL. Can be repeated.
- `--json` — Output as JSON array (default is a human-readable table)

**Behavior:**
- With `name`: prints just the value of the first matching cookie (useful for scripting)
- Without `name`: prints all cookies

**Output format (default):**

```
name=session_id  value=abc123  domain=.example.com  path=/  secure  httponly  expires=2025-01-01T00:00:00Z
name=theme       value=dark    domain=.example.com  path=/  session
```

Tab-separated fields, one cookie per line. Flags like `secure`/`httponly` only appear when true. `session` appears for session cookies (no expiry).

**Output format (--json):**

Full CDP cookie objects as a JSON array — includes all fields like `size`, `sourceScheme`, `sourcePort`, `sameSite`, `priority`, etc. This gives scripts access to every cookie attribute.

**Examples:**
```bash
# Get a specific cookie value (just the value, great for scripting)
TOKEN=$(rodney cookie-get session_id)

# List all cookies for the current page
rodney cookie-get

# Get cookies for a specific domain
rodney cookie-get --domain api.example.com

# Full JSON output for scripting
rodney cookie-get --json
rodney cookie-get --json session_id

# Combine with jq
rodney cookie-get --json | jq '.[] | select(.domain == ".example.com")'
```

**Implementation:** Calls `proto.NetworkGetCookies`. When `--domain` is provided, constructs `https://<domain>/` URLs to pass to CDP. When `--url` is provided, passes URLs directly. If neither is given, uses the CDP default (current page URLs). When a `name` argument is given without `--json`, prints only the value (like `rodney url` or `rodney title`). With `--json`, filters to matching cookies but outputs full JSON objects.

**Exit codes:**
- `0` — Success (cookies found, or listing returned empty list)
- `2` — Error (bad arguments, no browser)

### `rodney cookie-delete` — Delete cookies

```
rodney cookie-delete <name> [options]
```

**Options:**
- `--domain <domain>` — Only delete cookies with this exact domain
- `--url <url>` — Alternative to `--domain`: delete cookies matching this URL's domain and path
- `--path <path>` — Only delete cookies with this exact path

**Examples:**
```bash
# Delete a cookie by name (all domains)
rodney cookie-delete session_id

# Delete a cookie for a specific domain
rodney cookie-delete session_id --domain .example.com
```

**Implementation:** Calls `proto.NetworkDeleteCookies`. If only `name` is provided (no `--domain`, `--url`, or `--path`), the command first calls `NetworkGetCookies` to find all cookies with that name, then deletes each one (since CDP requires at least name + one of url/domain/path). This "delete by name everywhere" convenience avoids forcing users to know the exact domain.

### `rodney cookie-clear` — Delete all cookies

```
rodney cookie-clear
```

No arguments. Clears all browser cookies across all domains.

**Implementation:** Calls `proto.NetworkClearBrowserCookies`.

## Design Decisions

### Why separate commands instead of subcommands?

Rodney uses flat command names (`clear-cache`, `screenshot-el`, `ax-tree`). Following that convention, these are `cookie-set`, `cookie-get`, `cookie-delete`, `cookie-clear` rather than `cookie set`, `cookie get`, etc.

### Why `--domain` is emphasized over `--url`?

CDP accepts either a `url` or a `domain` parameter for cookie operations. Both are supported, but `--domain` is the recommended option in docs and examples because it's explicit — you know exactly what domain the cookie lands on. `--url` is a CDP convenience that infers domain, path, and scheme from the URL, which can be handy but is less obvious about what gets set. Both `--domain` and `--url` are accepted wherever CDP supports them.

### Why is `--domain` or `--url` required for cookie-set?

CDP's `Network.setCookies` will silently succeed but create a useless cookie if neither domain nor URL is provided. Making one required prevents confusion.

### Why tab-separated default output?

The default output mirrors the style of `rodney pages` (human-readable, one item per line). Tab separation makes it parseable with `cut` and `awk` while still being readable. The `--json` flag gives full fidelity for scripts that need every field.

### Why no `--all` flag on cookie-get?

`NetworkGetCookies` without URLs returns cookies for the current page. `NetworkGetAllCookies` exists in CDP but is deprecated. Instead, users can pass `--domain` or `--url` for specific domains they care about, which is more intentional. If we find users need "every cookie across all domains" we can add `--all` later.

## Help Text Addition

```
Cookies:
  rodney cookie-set <name> <val> [opts]  Set a cookie (--domain or --url required)
  rodney cookie-get [name] [--json]      Get cookies (name for value, omit for all)
  rodney cookie-delete <name> [opts]     Delete cookies by name
  rodney cookie-clear                    Clear all browser cookies
```

## Implementation Plan

1. Add four `case` entries in the main command switch: `cookie-set`, `cookie-get`, `cookie-delete`, `cookie-clear`
2. Implement `cmdCookieSet`, `cmdCookieGet`, `cmdCookieDelete`, `cmdCookieClear` functions
3. Add help text section
4. Add tests using the test HTTP server (set cookies, read them back, delete, verify gone)

### Test cases

- `cookie-set` + `cookie-get` round-trip: set a cookie, read it back by name, verify value
- `cookie-get` with `--json`: verify JSON output parses and contains expected fields
- `cookie-get` all: set multiple cookies, list them, verify all appear
- `cookie-get` with `--domain`: set cookies on different domains, verify filtering works
- `cookie-delete` by name: set cookie, delete it, verify `cookie-get` no longer returns it
- `cookie-delete` with `--domain`: set same-name cookies on different domains, delete one, verify the other remains
- `cookie-clear`: set multiple cookies, clear all, verify none remain
- Error cases: `cookie-set` without `--domain`/`--url`, `cookie-delete` without name

### Estimated size

~150-200 lines of implementation code, ~100-150 lines of tests.
