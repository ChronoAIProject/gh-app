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

## Install with Homebrew

Install from the project tap:

```bash
brew install ChronoAIProject/tap/gh-app
```

The formula builds from source, so Go is a build dependency, and its test block asserts that the installed binary reports the tagged version. Remove it with `brew uninstall gh-app && brew untap ChronoAIProject/tap`.

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

## Enable it globally

Three separate pieces have to be in place. They are independent — each can be installed,
verified and removed on its own, and doing only some of them is a valid choice.

**1. Configuration.** Create `~/.config/gh-app/config.toml` as described above, then check
that the Apps resolve:

```bash
gh-app status
```

It prints each App and the installations GitHub reports for it. If that fails, nothing
below will work.

**2. Git.** This is what makes `git clone`, `fetch` and `push` use App credentials:

```bash
gh-app git-install --global
```

It prints the resulting helper chain before writing it. Without `--global` the helper is
installed for the current repository only, which is the safer default when trying it out.

**3. `gh`.** This is a separate mechanism — `gh` keeps its own token store and never
consults Git's credential helpers, so step 2 does not affect it. Add to `~/.zshrc`
(or `~/.bashrc`, with `bash` in place of `zsh`):

```bash
command -v gh-app >/dev/null 2>&1 && eval "$(gh-app shell-init zsh)"
```

Open a new shell afterwards. Read the section below on what this changes before keeping it
— for some commands it changes their meaning, not just the token behind them.

### Verifying it took effect

Identity depends on where you are, so check from two places:

```bash
cd <a repository an App reaches>
git credential fill <<< $'protocol=https\nhost=github.com\npath=OWNER/REPO.git\n'  # username=x-access-token
gh api rate_limit --jq .rate.limit                                                # 15000

cd /tmp
gh api user --jq .login                                                           # your own login
gh api rate_limit --jq .rate.limit                                                # 5000
```

The `path` line is required. The helper takes the repository from the credential request itself,
not from the working directory, so a request without one matches nothing and Git falls through to
your personal credentials — which is indistinguishable from a setup that did not work. Real Git
operations supply the path because `git-install` sets `useHttpPath`.

Commits still show you as their author — that is expected and does not mean the setup
failed. See *What the App identity does and does not change* below.

### Removing it

Each piece comes out independently:

```bash
git config --global --unset-all credential.https://github.com.helper   # step 2
git config --global --unset credential.https://github.com.useHttpPath  # step 2
# delete the eval line from ~/.zshrc, then open a new shell            # step 3
rm -rf ~/.config/gh-app                                                # step 1, config and cache
```

Removing the Git helper leaves the chain empty for `github.com`, so Git falls back to
whatever your system configuration provides — on macOS that is usually `osxkeychain`. If
you had `gh auth setup-git` before, run it again to restore that entry explicitly.

## Use with gh

Try it in the current shell first:

```bash
eval "$(gh-app shell-init zsh)" # use bash for bash
gh repo view
```

To keep it, add the same line to `~/.zshrc` (or `~/.bashrc`):

```bash
command -v gh-app >/dev/null 2>&1 && eval "$(gh-app shell-init zsh)"
```

The shell function infers `origin` from the current Git repository. It delegates unchanged to personal `gh` credentials when no App matches, when repository context is unavailable, when `GH_APP_DISABLE=1`, or when `GH_TOKEN` is already set.

### What changes once the function is active

Inside a repository an App reaches, `gh` runs as the App installation rather than as you.
Everywhere else — outside a Git repository, in a repository no App reaches, or behind an
SSH remote — nothing changes at all.

That identity swap is not cosmetic. Measured against a live installation:

| command | as you | as the App |
|---|---|---|
| `gh api rate_limit` | 5000/hour | 15000/hour |
| `gh api user` | your login | **fails, HTTP 403** |
| `gh repo list` | *your* repositories | **the installation's repositories** |
| `gh repo view`, `pr list`, `issue list`, `release list`, `auth status` | unchanged | unchanged |

Two of those deserve attention.

`gh api user` fails because an installation token does not represent a user. Any command
that resolves "the authenticated user" fails the same way. This is loud — you will see it.

`gh repo list` is the quiet one. It succeeds and returns a plausible list, but the list is
of repositories the *installation* can reach, not yours. Nothing signals the difference.
If a command's meaning depends on who is asking, check which identity is in effect before
trusting its output.

Either escape hatch restores your own credentials for a single command:

```bash
GH_APP_DISABLE=1 gh repo list
GH_TOKEN="$(gh auth token)" gh api user
```

For scripts:

```bash
gh-app token --target OWNER/REPO
GH_APP_TARGET=OWNER/REPO gh-app token --auto
```

Both accept `HOST/OWNER/REPO` when the target lives on a GitHub Enterprise Server host declared in `config.toml`; the two-segment form assumes `github.com`.

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

Both modes also set `credential.https://<host>.useHttpPath` to `true`. Git omits the repository path from credential requests by default, and the helper resolves the App from that path, so without this setting nothing would ever match.

`gh-app` returns nothing for a repository no App reaches, so git falls through to `gh` and those repositories keep working exactly as before. If `gh` is not on `PATH` the fallback is omitted and a warning says so.

Then use HTTPS remotes normally:

```bash
git clone https://github.com/OWNER/REPO.git
git fetch
git push
```

The helper generates or refreshes a GitHub App installation token when Git asks for credentials.

### What the App identity does and does not change

Three identities are involved in a push, and the App credential governs only one of them.

| | Where it comes from | Affected by gh-app |
|---|---|---|
| Commit **author** | your local `git config user.name` / `user.email` | no |
| Commit **committer** | the same, or `GitHub` when a merge is made on the web | no |
| **Pusher** | the credential the helper supplies | **yes** |

Author and committer are recorded inside the commit object when it is created. Changing
which credential pushes it later cannot rewrite them, so commits keep showing you as their
author on GitHub. That is the intended result: the App governs *who may write to the
repository*, not *who wrote the code*.

The pusher is what changes. After `git-install`, GitHub records pushes to App-covered
repositories as the App's bot account, which is what appears in branch protection rules,
audit logs, and repository activity. Note that bot pushes are not reported through the
public events API — `gh api repos/OWNER/REPO/activity` shows them, `.../events` does not,
so the events endpoint can make it look as though nothing changed.

Setting `user.email` to the bot address would make commits appear authored by the bot too,
but that misattributes work you did to an automation account and is not recommended.

### Which identity a given action uses

Git and `gh` reach credentials by different routes, and only one of those routes is always
active. Git consults the credential helper on every operation, wherever it runs. `gh` gets
the App token from the shell function — so the question for any `gh` command is simply
whether that function is defined in the shell running it.

| action | credential route | identity |
|---|---|---|
| `git push`, `fetch`, `clone` | credential helper | the App |
| `gh …` where the function is defined | function injects `GH_TOKEN` | the App |
| `gh …` where it is not | `gh`'s own token store | you |
| `GH_TOKEN=… gh …` | the token you supplied | whoever that token belongs to |

Being non-interactive is not itself the deciding factor, though it is the most common
reason: zsh reads `~/.zshrc` only for interactive shells, so `zsh -c` does not get the
function while `zsh -i -c` does. A script that sources the rc file, or inherits an
environment where the function is already defined, uses the App like any terminal would.

The other case is a shell that started before the function was installed. A long-lived
session, or a tool that snapshots your environment at launch and reuses it, keeps running
with the definitions it captured — adding the line to `~/.zshrc` afterwards does nothing
for it until it is restarted or the file is sourced again.

So a pull request may show your account even though its branch was pushed by the App. That
is not a broken setup: `git push` went through the helper, and that `gh` invocation did not
go through the function.

To confirm which is in effect, ask the shell rather than guessing:

```bash
type gh    # "a shell function" means the App path is available here
```

Scripts that want the App identity should not rely on the function being present. Pass the
token explicitly:

```bash
GH_TOKEN="$(gh-app token --target OWNER/REPO)" gh pr create …
```

## Other commands

```bash
gh-app status   # list each App and its reachable installations
gh-app clear    # clear the unified cache
gh-app migrate  # explicitly convert legacy config.json
gh-app version  # print the version derived from the release tag
```

## Security

- `gh-app` refuses to use a private key readable by group or other. Keys downloaded from GitHub may arrive group- or other-readable (commonly `0644`), so run `chmod 600 private-key.pem` before first use.
- Each computer holding the private key can act as the GitHub App installation within its granted permissions.
- Prefer a separate GitHub App or private key per trust boundary. GitHub App private keys can be revoked independently.
- Configuration and cache files use mode `0600`; their directory uses `0700`.
- Cached installation tokens are keyed by host, owner, and repository and expire after approximately one hour.
