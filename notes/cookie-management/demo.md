# Cookie Management Demo

*2026-03-05T20:13:42Z by Showboat 0.6.1*
<!-- showboat-id: 0f1de88a-67b0-4704-8aa1-282abaa3ea2f -->

Rodney now has four cookie management commands: `cookie-set`, `cookie-get`, `cookie-delete`, and `cookie-clear`. These use Chrome DevTools Protocol under the hood. Let's walk through them.

## Setting up

First, let's navigate to a page and confirm the browser is running.

```bash
rodney status
```

```output
Browser running (PID 30198)
Debug URL: ws://127.0.0.1:31199/devtools/browser/dac1132f-b520-4a67-9600-4c682d74f0b8
Pages: 0
Active page: 0
```

```bash
rodney open https://www.example.com/
```

```output
Example Domain
```

## cookie-set: Setting cookies

Set a cookie with `--domain`. This is the recommended approach — it's explicit about where the cookie lands.

```bash
rodney cookie-set session_id abc123 --domain .example.com
```

```output
```

Set a second cookie with more flags — secure, httponly, and a custom path:

```bash
rodney cookie-set token secret123 --domain .example.com --path /api --secure --httponly --samesite Strict
```

```output
```

You can also use `--url` instead of `--domain`. CDP infers the domain, path, and scheme from the URL:

```bash
rodney cookie-set theme dark --url https://www.example.com/
```

```output
```

## cookie-get: Reading cookies

With no arguments, lists all cookies for the current page:

```bash
rodney cookie-get
```

```output
name=theme	value=dark	domain=www.example.com	path=/	secure	session
name=session_id	value=abc123	domain=.example.com	path=/	session
```

Note: the `token` cookie doesn't appear because it's on `/api` and we're on `/`. CDP only returns cookies that would be sent with a request to the current page URL.

Pass a cookie name to get just its value — great for scripting:

```bash
rodney cookie-get session_id
```

```output
abc123
```

Use `--domain` to query cookies for a domain you're not currently on:

```bash
rodney cookie-get --domain example.com
```

```output
name=session_id	value=abc123	domain=.example.com	path=/	session
```

Use `--json` for full CDP cookie objects — useful with `jq`:

```bash
rodney cookie-get --json session_id
```

```output
[
  {
    "name": "session_id",
    "value": "abc123",
    "domain": ".example.com",
    "path": "/",
    "expires": -1,
    "size": 16,
    "httpOnly": false,
    "secure": false,
    "session": true,
    "priority": "Medium",
    "sameParty": false,
    "sourceScheme": "NonSecure",
    "sourcePort": 80
  }
]
```

## cookie-delete: Removing specific cookies

Delete a cookie by name. Without `--domain`, it finds and deletes all cookies with that name across all domains:

```bash
rodney cookie-delete theme
```

```output
```

Confirm it's gone:

```bash
rodney cookie-get
```

```output
name=session_id	value=abc123	domain=.example.com	path=/	session
```

The `theme` cookie is gone, only `session_id` remains.

You can also scope deletion with `--domain` or `--path`:

```bash
rodney cookie-delete session_id --domain .example.com
```

```output
```

```bash
rodney cookie-get
```

```output
```

No output — all cookies are gone.

## cookie-clear: Nuclear option

Set a few cookies, then clear everything at once:

```bash
rodney cookie-set a 1 --domain .example.com && rodney cookie-set b 2 --domain .example.com && rodney cookie-set c 3 --url https://other.example.com/ && rodney cookie-get --domain example.com
```

```output
name=b	value=2	domain=.example.com	path=/	session
name=a	value=1	domain=.example.com	path=/	session
```

```bash
rodney cookie-clear
```

```output
All cookies cleared
```

```bash
rodney cookie-get --domain example.com
```

```output
```

No cookies remain — `cookie-clear` wipes everything across all domains.

## Defaulting to the current page

If you omit `--domain` and `--url`, `cookie-set` defaults to the current page's URL — just like `document.cookie=` in JavaScript:

```bash
rodney cookie-set simple_cookie works
```

```output
```

```bash
rodney cookie-get simple_cookie
```

```output
works
```

The cookie was set on `www.example.com` (the current page's host) automatically. Let's verify with `--json`:

```bash
rodney cookie-get --json simple_cookie
```

```output
[
  {
    "name": "simple_cookie",
    "value": "works",
    "domain": "www.example.com",
    "path": "/",
    "expires": -1,
    "size": 18,
    "httpOnly": false,
    "secure": true,
    "session": true,
    "priority": "Medium",
    "sameParty": false,
    "sourceScheme": "Secure",
    "sourcePort": 443
  }
]
```

## cookie-clear --domain: Scoped clearing

You can also clear cookies for just one domain, leaving others untouched:

```bash
rodney cookie-clear && rodney cookie-set keep_me yes --domain .example.com && rodney cookie-set remove_me bye --domain .other.com && rodney cookie-get --domain example.com && rodney cookie-get --domain other.com
```

```output
All cookies cleared
name=keep_me	value=yes	domain=.example.com	path=/	session
name=remove_me	value=bye	domain=.other.com	path=/	session
```

```bash
rodney cookie-clear --domain other.com
```

```output
Cookies cleared for other.com
```

The `.example.com` cookie survives, the `.other.com` one is gone:

```bash
rodney cookie-get --domain example.com && rodney cookie-get --domain other.com
```

```output
name=keep_me	value=yes	domain=.example.com	path=/	session
```
