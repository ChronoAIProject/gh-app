# gh-app

Resolve repository-scoped GitHub App installations for GitHub CLI (`gh`) and HTTPS Git, with personal credentials as the fallback when no App matches.

## Features

- multiple GitHub Apps and installations selected by repository
- RS256 GitHub App JWT generation
- repo-keyed installation-token caching and refresh 5 minutes before expiry
- opt-in shell integration that leaves existing `GH_TOKEN` values untouched
- Git credential-helper integration for clone/fetch/pull/push
- GitHub.com and GitHub Enterprise API/host configuration
- one Go binary with a strict TOML configuration

## Build

```bash
make build
```

The supported release platforms are macOS on arm64 and amd64, and Linux on arm64 and amd64. Windows is excluded because the cache's advisory file locking uses Unix `flock` semantics and the program does not compile for Windows. `make dist` compiles all four advertised targets into `dist/`, named for the `gh` extension convention (`gh-app-<os>-<arch>`). Both `make build` and `make dist` stamp `gh-app version` from the current Git tag, falling back to `dev` when no tag describes the checkout.

## Tests

```bash
make test
```

The suite runs offline and needs no credentials: it generates its own RSA keys and serves GitHub responses through hermetic `httptest` request/response fixtures.

One thing it cannot cover is whether GitHub accepts the JWT the binary mints — a local test server replies to whatever it is sent. To check that against the real API, copy the template and fill in an installation:

```bash
cp .env.example .env
make test-e2e
```

`.env` is git-ignored and references the App ID, target owner/repository, and private-key path. `.env.example` documents each value and covers GitHub Enterprise Server.

The live tests skip when the variables are absent and fail loudly when only some are set. Each run mints a real installation token, which is read-only and expires after about an hour. `make test` clears these variables, so it stays offline regardless of `.env`.

## Install as a gh extension

From GitHub, once a release is published:

```bash
gh extension install ChronoAIProject/gh-app
```

From a local clone:

```bash
gh extension install .
```

The repository directory must be named `gh-app`, and the root must contain the `gh-app` executable. Alternatively place the binary on `PATH` and use `gh-app` directly.

## Configure

Create `~/.config/gh-app/config.toml`:

```toml
[[apps]]
app_id = 123456
private_key = "~/.config/gh-app/keys/work.pem"
owners = ["MyOrganization"]

[[apps]]
app_id = 789012
private_key = "~/.config/gh-app/keys/enterprise.pem"
host = "github.example.com"
api_url = "https://github.example.com/api/v3"
```

The private key is referenced by absolute path and is never copied, so it can live anywhere; `~/.config/gh-app/keys/` is only a suggestion that keeps everything this tool needs under one directory. `gh-app clear` removes only `cache.json` and the lock file, so keys stored alongside them are not touched.

Unknown keys are rejected. Installation IDs are discovered from GitHub at resolution time. The unified cache is `~/.config/gh-app/cache.json`. `GH_APP_CONFIG_DIR` relocates both files; otherwise `$XDG_CONFIG_HOME/gh-app` is used when set, followed by `~/.config/gh-app`. Migrate an old singleton JSON configuration explicitly with `gh-app migrate`.

```bash
GH_APP_CONFIG_DIR=~/.config/gh-app/staging gh-app status
```

The private key is referenced by path and is not copied.

## Use with gh

```bash
eval "$(gh-app shell-init zsh)" # use bash for bash
gh repo view
```

The shell function infers `origin` from the current Git repository. It delegates unchanged to personal `gh` credentials when no App matches, when repository context is unavailable, when `GH_APP_DISABLE=1`, or when `GH_TOKEN` is already set. For scripts:

```bash
gh-app token --target OWNER/REPO
GH_APP_TARGET=OWNER/REPO gh-app token --auto
```

Git reports rejected credentials back through the helper and invalidates that repository's cached token. The shell-wrapped `gh` process does not expose GitHub HTTP status to the wrapper; after access is revoked, run `gh-app clear` to force revalidation before the normal five-minute refresh margin.

## Use with Git

Install the helper in the current repository (the default):

```bash
gh-app git-install
```

Use `gh-app git-install --global` only when global interception is intended; it previews the resulting helper chain first. Both modes reset inherited helpers for configured hosts, then install `gh-app` followed by `gh`'s own helper:

```
helper =                              # reset
helper = !"…/gh-app" credential       # repositories an App reaches
helper = !…/gh auth git-credential    # everything else, as your own account
```

`gh-app` returns nothing for a repository no App reaches, so git falls through to `gh` and those repositories keep working exactly as before. If `gh` is not on `PATH` the fallback is omitted and a warning says so.

Then use HTTPS remotes normally:

```bash
git clone https://github.com/OWNER/REPO.git
git fetch
git push
```

The helper generates or refreshes a GitHub App installation token when Git asks for credentials.

## Other commands

```bash
gh-app status   # list each App and its reachable installations
gh-app clear    # clear the unified cache
gh-app migrate  # explicitly convert legacy config.json
gh-app version  # print the version derived from the release tag
```

## Security

- Protect the PEM file with OS file permissions, e.g. `chmod 600 private-key.pem`.
- Each computer holding the private key can act as the GitHub App installation within its granted permissions.
- Prefer a separate GitHub App or private key per trust boundary. GitHub App private keys can be revoked independently.
- Configuration and cache files use mode `0600`; their directory uses `0700`.
- Cached installation tokens are keyed by host, owner, and repository and expire after approximately one hour.
