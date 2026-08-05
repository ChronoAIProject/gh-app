# gh-app

Use a GitHub App installation transparently with both GitHub CLI (`gh`) and HTTPS Git.

## Features

- `gh app ...` extension command or standalone `gh-app`
- RS256 GitHub App JWT generation
- installation-token caching and automatic refresh 5 minutes before expiry
- transparent `GH_TOKEN` injection for arbitrary `gh` commands
- Git credential-helper integration for clone/fetch/pull/push
- GitHub.com and GitHub Enterprise API/host configuration
- no runtime dependencies; one Go binary

## Build

```bash
go build -o gh-app ./cmd/gh-app
```

## Install as a gh extension

From a local clone:

```bash
gh extension install .
```

The repository directory must be named `gh-app`, and the root must contain the `gh-app` executable. Alternatively place the binary on `PATH` and use `gh-app` directly.

## Configure

```bash
gh app init \
  --app-id 123456 \
  --installation-id 78901234 \
  --key ~/.config/github-app/private-key.pem
```

For GitHub Enterprise Server:

```bash
gh app init \
  --app-id 123456 \
  --installation-id 78901234 \
  --key ~/.config/github-app/private-key.pem \
  --host github.example.com \
  --api-url https://github.example.com/api/v3
```

The configuration is stored under the operating system user config directory. The private key is referenced by path and is not copied.

## Use with gh

```bash
gh app exec -- gh repo view OWNER/REPO
gh app exec -- gh pr list --repo OWNER/REPO
gh app exec -- gh api repos/OWNER/REPO
```

Print a token for shell integration:

```bash
export GH_TOKEN="$(gh app token)"
gh pr list --repo OWNER/REPO
```

## Use with Git

Install the helper globally once:

```bash
gh app git-install
```

Then use HTTPS remotes normally:

```bash
git clone https://github.com/OWNER/REPO.git
git fetch
git push
```

The helper generates or refreshes a GitHub App installation token when Git asks for credentials.

## Other commands

```bash
gh app status   # validate configuration and show token expiry
gh app clear    # clear cached installation token
```

## Security

- Protect the PEM file with OS file permissions, e.g. `chmod 600 private-key.pem`.
- Each computer holding the private key can act as the GitHub App installation within its granted permissions.
- Prefer a separate GitHub App or private key per trust boundary. GitHub App private keys can be revoked independently.
- The cached installation token is written with user-only permissions and expires after approximately one hour.
