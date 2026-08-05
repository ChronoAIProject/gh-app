# gh-app: repository-scoped multi-App credential resolver

Status: converged (sshx meta-layer convergence, 4 propose / 2 revise)
Date: 2026-08-05

## 1. What this is, stated honestly

`gh-app` becomes an **App-first credential broker keyed by repository**, not a global
replacement for personal GitHub credentials.

The earlier framing ("replace gh credentials globally with a GitHub App") is factually
wrong and the spec must not repeat it. Two measured facts kill it:

- Under an installation token, `gh api user` returns `403 Resource not accessible by
  integration`. An installation token is not a user identity, so it cannot be a
  drop-in substitute for one.
- The chosen fallback keeps `gh auth token` as the backstop when no App matches, so
  `gh auth login` remains required.

The honest purpose: **globally intercept `gh` and Git credential requests, select an
App installation when the repository context resolves to one, and otherwise delegate
unchanged to the personal credential.**

## 2. Root cause being fixed

The current model binds one `(app_id, installation_id, private_key)` tuple directly to
token minting. Repository context never participates. But authorization on GitHub is
**repository-scoped**, and App vs installation are independent axes (one App can have
many installations). Any design that keeps the singleton and bolts probing onto it
patches a symptom.

The missing thing is a single **resolver**: a request-scoped, evidence-based mapping
from `(host, owner, repo)` to `(App, discovered installation, token)`.

## 3. Architecture: one producer, three thin consumers

```
                     ┌──────────────────────────────┐
   gh (shell func) ──▶                              │
   git (credential) ─▶   resolver  (owns policy)    │──▶ token | no-match
   gh-app token    ──▶                              │        | ambiguous
                     └──────────────────────────────┘        | error
                                    │
                     config.toml ───┘
                     cache (single file, atomic replace)
```

**The resolver owns**: App selection, installation discovery, tie-breaking, cache
validity, token minting and refresh. It accepts an explicit normalized target and
returns a classified outcome. It never guesses context.

**Consumers own their own context acquisition and nothing else:**

- shell function: reads the current directory's git remote, calls the resolver, and on
  `no-match` / `no-context` invokes `command gh` unchanged.
- credential adapter: parses the git credential protocol (including the `path=` field
  git already supplies because `useHttpPath=true` is set), calls the resolver.
- `gh-app token`: takes an explicit target for scripting.

**Boundary rules** (the locus dyad resolved):

- Consumers must NOT reimplement routing policy.
- The resolver must NOT parse `gh` subcommand flags, must NOT choose which git remote
  is authoritative, and must NOT read `gh`'s personal credential store.

## 4. Configuration

`~/.config/gh-app/config.toml` by default on supported macOS and Linux systems. Windows is intentionally unsupported: the cache's advisory locking relies on Unix `flock` semantics, so the program does not compile for Windows. `GH_APP_CONFIG_DIR` takes precedence, followed by `$XDG_CONFIG_HOME/gh-app` when set. Strict decoding — unknown keys are an error.

```toml
[[apps]]
app_id      = 4483923
private_key = "~/.config/gh-app/keys/chrono.pem"
owners      = ["ChronoAIProject"]   # optional: ordering + tie-break only, never authority

[[apps]]
app_id      = 5567890
private_key = "~/.config/gh-app/keys/personal.pem"

[[apps]]
app_id      = 12345
private_key = "~/.config/gh-app/keys/work.pem"
host        = "github.example.com"
api_url     = "https://github.example.com/api/v3"
```

Deliberately absent, each removed by a specific objection:

- **no `installation_id`** — discovered at runtime.
- **no `default_app`** — "no context" must delegate to plain `gh`, not guess an App.
- **no `fallback_gh` toggle** — fallback on explicit `no-match` is the only sane
  behavior; a switch for it is unearned machinery.
- **no `name` field** — nothing needs to reference an App by name; `app_id` identifies it.

`owners` orders probing and breaks ties. It never grants access on its own — the probe
is the authority.

## 5. Resolution algorithm

Input: `(host, owner, repo?)`, or an explicit `GH_APP_TARGET` override.

1. **Cache lookup** on key `(host, owner, repo)`, qualified by the App IDs in the
   currently loaded configuration. A row whose `app_id` is no longer configured is a
   miss. Otherwise, a hit with a token more than 5 minutes from expiry → return it.
2. **Candidate ordering**: Apps matching `host`; those listing `owner` in `owners` first,
   then the rest in config order.
3. **Probe** each candidate with an App JWT: `GET /repos/{owner}/{repo}/installation`
   (or `/orgs/{org}/installation` when no repo is known).
   - `200` → this App owns the target; record the installation id.
   - `404` → this App does not reach the target; try the next.
4. **Multiple 200s** → `ambiguous`. Fail closed with both app_ids and instruct the user
   to disambiguate via `owners`. Never silently pick the first.
5. **All 404** → `no-match`.
6. Mint the installation token, write the cache entry atomically, return it.

**Outcomes are a closed set** and callers switch on them:
`ok | no-match | no-context | ambiguous | config-error | operational-error`.

`no-match` and `operational-error` must be distinguishable — a network failure must
never be silently reported as "no App covers this repo".

### The 404 honesty rule

`GET /repos/{o}/{r}/installation` returns 404 both when the App lacks access and when
the repo does not exist; GitHub does this deliberately so private-repo existence cannot
be probed. Therefore exhausted probes mean exactly: *no configured App established
access to this target.* Error text must not claim the repo does not exist, nor that
access was denied. Stating either would be a fabricated diagnosis.

## 6. Cache: one file, not two

Key `(host, owner, repo)` → `(app_id, installation_id, token, expires_at)`.

A single cache serves routing and tokens together. A separate route cache was rejected:
it would be a second source of truth for the same decision.

- **Keyed by repo, not owner.** An installation may be `repos: selected`. An owner-level
  key would hand out a token that then 403s on an unauthorized repo — the cache would be
  asserting authorization it never verified.
- **No TTL.** A 24-hour TTL was rejected as an unearned constant. Entries are revalidated
  at natural token refresh. Git's credential rejection feedback invalidates the matching
  entry through the helper's `erase` operation. The shell-wrapped `gh` consumer cannot
  observe `gh`'s HTTP status, so it cannot invalidate a rejected token before refresh.
- **One bounded lock transaction.** Every cache read, insertion, invalidation, and clear
  takes the same exclusive advisory flock before touching the file. Acquisition retries
  non-blockingly for at most one second, so a dead holder is released by the kernel and
  a live stalled holder cannot hang the caller forever.
- **Atomic replacement** (write temp + rename) happens inside that transaction, so
  concurrent read-modify-write operations cannot lose an insertion or resurrect a
  deletion.
- Lock timeout is caller-sensitive: lookup is an `operational-error` that makes the Git
  helper and shell function delegate to their existing personal-credential path; a token
  already minted is returned even when its cache insertion times out; credential
  invalidation and explicit `clear` fail visibly because claiming either deletion
  succeeded would be unsafe. The shell distinguishes cache contention with exit status
  75; ambiguous and configuration errors remain visible failures.
- File mode `0600`, directory `0700`, matching the existing guarantees.

## 7. Git credential helper and the pre-existing chain

Measured: on macOS `/Library/Developer/CommandLineTools/usr/share/git-core/gitconfig`
sets `credential.helper=osxkeychain` unconditionally, with no URL scope. Git helper
config is **additive and ordered**, so that helper runs *before* any user-level helper
and can answer first from its cache. A design assuming "our helper is the only helper"
is simply wrong.

Therefore `git-install`:

- is **repository-local by default** (`git config --local`), because a global helper
  binds every HTTPS operation on the machine;
- global mode requires an explicit flag, prints the exact resulting chain first, and is
  reversible;
- resets the inherited chain with an empty `helper =` entry before appending gh-app, so
  gh-app is consulted first;
- **appends gh's own helper after gh-app**, restoring the personal-credential backstop
  the reset removed. Section 1 defines this tool as App-first *with* a personal fallback;
  a reset that drops gh's entry and stops there delivers only the first half, leaving any
  repository no App reaches with no credential source at all. gh-app returns nothing for
  such a target, so git moves on to the next helper — which must exist. When `gh` is not
  on `PATH` the fallback is omitted and a warning says so, rather than silently
  installing a chain that answers for some repositories and not others;
- keeps emitting `password_expiry_utc`, without which osxkeychain caches a token past
  its one-hour life.

The resulting chain, in the order git consults it:

```
helper =                              # reset: drops the unconditional system helper
helper = !"…/gh-app" credential       # App-covered targets
helper = !…/gh auth git-credential    # everything else, as your own account
```

## 8. Shell integration

`gh-app shell-init` emits a function for the user's shell:

```
gh() {
  local t resolver_status
  t="$(gh-app token --auto)"
  resolver_status=$?
  if [ "$resolver_status" -eq 75 ]; then command gh "$@"; return; fi
  if [ "$resolver_status" -ne 0 ]; then return "$resolver_status"; fi
  if [ -n "$t" ]; then GH_TOKEN="$t" command gh "$@"; else command gh "$@"; fi
}
```

Measured cost: token resolution on a warm cache is ~6ms, reading the git remote ~10ms,
versus ~30ms for `gh` reading its own keyring. No daemon is justified.

Degenerate cases, each delegating to plain `gh` rather than guessing:

| situation | behavior |
|---|---|
| not in a git repository | no context → plain `gh` |
| remote is SSH, or a non-GitHub host | no context → plain `gh` |
| multiple remotes | `origin` if present, else no context |
| resolver returns `no-match` | plain `gh` (personal token backstop) |
| cache lock times out | operational-error status 75 → plain `gh` |
| resolver returns `ambiguous` or `config-error` | error is visible on stderr, not swallowed |
| `GH_APP_DISABLE=1` | bypass entirely |
| `GH_APP_TARGET=owner/repo` | explicit override wins over remote inference |
| `GH_TOKEN` already set by the user | left untouched |

Sourcing is opt-in. The function is the only global interception point, and it is inert
whenever context does not resolve.

## 9. Command surface

| command | change |
|---|---|
| `token [--auto\|--target o/r]` | resolves and prints; `--auto` uses cwd remote |
| `credential` | unchanged protocol, now repo-aware via `path=` |
| `git-install [--global]` | repo-local default; chain reset; preview |
| `shell-init [bash\|zsh]` | new |
| `status` | lists configured Apps and, per App, its reachable installations |
| `clear` | clears the unified cache |
| `migrate` | new: one-shot JSON → TOML, explicit |
| `init` | **removed** — it encodes the singleton model |
| `exec` | **removed** — superseded by the shell function |

`migrate` is explicit rather than automatic: silently rewriting a user's credential
configuration on next run is a hidden side effect. A stale JSON config produces an
error naming `migrate`.

## 10. TOML dependency

Go's standard library has no TOML parser. The binary currently has **zero** third-party
dependencies — a real property being traded.

Decision: take one small, pinned, mature TOML library. A hand-written parser was
rejected: it is a second-rate implementation of a solved problem, and its edge cases
become our maintenance burden forever.

**Chosen: `github.com/pelletier/go-toml/v2` v2.4.3.** Measured, not assumed:

| candidate | transitive deps | strict decoding |
|---|---|---|
| BurntSushi/toml v1.6.0 | 0 | returns `err=nil` on an unknown key; requires a manual `md.Undecoded()` check plus hand-built error |
| **pelletier/go-toml/v2 v2.4.3** | **0** | `DisallowUnknownFields()` — one call, real error |

Both add zero transitive dependencies, so the cost of leaving zero-dependency status is
one direct module, not a dependency tree.

The tie-breaker is a credential-safety property. In the measured test, a config
containing `privat_key` (a misspelling of `private_key`) decoded **without error** under
BurntSushi — the App would silently end up with no private key. Under pelletier it fails
with `strict mode: fields in the document are missing in the target struct`. For a tool
whose entire job is selecting the right credential, silently ignoring a mistyped key
field is unacceptable.

## 11. Testing

The existing suite is hermetic: it generates its own RSA keys, serves the GitHub API
from `httptest`, and `TestMain` fails the run if any test writes to the real config
directory. Those properties are load-bearing and must survive.

- The `TestMain` guard must be widened beyond `config.json` / `token-cache.json` to
  cover the new filenames, or it silently stops guarding.
- Obsolete assertions tied to `init` / `exec` / JSON get updated, keeping their
  regression intent.

New hermetic coverage required:

1. Routing picks the right App among several when only one probe returns 200.
2. Two 200s produce `ambiguous`, not a silent first-match.
3. All-404 produces `no-match`, distinct from a network error.
4. `owners` reorders probing and breaks ties, but never grants access without a probe.
5. Repo-keyed cache: a `repos: selected` installation authorized for repo A does not
   serve a token for repo B under the same owner.
6. Cache invalidation through the credential helper's `erase` operation, and refresh
   inside the 5-minute margin. HTTP-status-specific invalidation is not promised: Git
   reports rejected credentials through `erase`, while the shell wrapper cannot observe
   `gh`'s HTTP response status.
7. A clear racing a cache writer cannot restore the cleared snapshot; a live holder of
   the cache flock causes bounded, caller-specific timeout behavior rather than a hang.
8. Atomic cache replacement under concurrent writers.
9. Strict TOML decoding rejects unknown keys.
10. `migrate` converts a JSON config and refuses to clobber an existing TOML.
11. `git-install` resets the inherited helper chain and defaults to repo-local.
12. Shell function degenerate cases (table in §8) via the resolver's classified outcomes.

## 12. Explicitly out of scope

- Committing, pushing, tagging, releasing, or any GitHub lifecycle mutation.
- Writing tokens into `gh`'s keyring.
- A background daemon (measurements show it is unnecessary).
- Parsing `gh` subcommand flags to infer the target repository.

## 13. Unresolved, carried forward

- Whether `/orgs/{org}/installation` shares the exact 404 ambiguity of the repo
  endpoint is inferred, not measured.
- Whether every git/platform version honors `password_expiry_utc` is inferred from one
  measured platform.

---

# 14. The long-term stable form (sshx run 2 — 6-seat convergence)

Five review rounds converged on code that three independent reviewers approved. This
section addresses a different question: is that correctness held by **structure** or by
**convention**? All six philosopher seats independently answered "convention", and all six
proposed the same structural fix.

## 14.1 The threat, named by all six seats

> A future maintainer can call `readCacheUnlocked` / `writeCacheAtomic` without
> `withCacheLock`, or nest `withCacheLock` inside itself, reintroducing the lost update of
> round 3 or a one-second self-timeout. No current production path does so — because five
> review rounds manually walked every path into the transaction.

Nothing structural prevents path number ten from skipping it. Correctness is currently a
property of the review history, not of the code.

## 14.2 Decision: a package boundary, not a file-layout change

The cache moves into an internal package that exports **only semantic operations**:

    Get(target) (Entry, bool)
    Put(entry) error
    Invalidate(target) error
    Clear() error

Not exported, and therefore **not callable from the command package at all**:
the on-disk representation, raw read/write primitives, lock acquisition, and any
transaction callback or lock handle.

This is the whole point: Go's package visibility is enforced by the compiler. A future
maintainer does not need to know the protocol, because the primitives that could violate
it are unreachable. Convention becomes structure.

## 14.3 Rejected: one file per target

Two seats proposed replacing the single JSON array with `cache/<hash(target)>.json`, one
file per entry, on the grounds that it removes whole-file read-modify-write and therefore
the lock, the timeout, and the per-caller policy.

Three seats independently refuted it, and they are right:

- `clear` implemented as "remove the directory" races a concurrent writer that recreates
  an entry file immediately afterward. Credentials the user explicitly cleared come back.
- Same-target `invalidate` versus `mint` is still a race: the deleting process removes the
  file while the minting process writes it.

Per-target files remove *cross-target* lost updates only. They do not remove *same-target*
ordering problems or `clear` linearization, which are exactly the failures rounds 3 and 4
existed to fix. Adopting it would trade a solved problem for an unsolved one.

The ownership seat's actual requirement — "expose only per-entry Get/Put/Delete/Clear
operations" — is a statement about the **API shape**, and the package boundary satisfies it
completely. The file layout was a means, not the end.

## 14.4 Reads take no lock

`rename` is atomic, so a reader observes either the complete previous file or the complete
next one — never a partial state. A read therefore needs no exclusion.

Consequences, all in the direction of less mechanism:

- `Get` never blocks on an unrelated writer.
- The lock timeout, its conversion to exit status 75, and the shell fallback path become
  **unreachable from the read path**. Unreachable code is deleted, not kept "just in case".
- Writes keep the flock: they are still read-modify-write on the whole file, and losing a
  deletion is still a correctness failure, not a cheap re-probe.

## 14.5 The untested link

Five seats independently flagged the same gap: the shell tests **inject** exit status 75
rather than causing a real lock timeout to produce it. The chain
`real timeout -> cacheFallbackError -> main exit 75 -> shell delegates` is never exercised
end to end, so a regression anywhere in it leaves every current test green.

A hermetic subprocess test must hold a real flock, invoke the real command entry point,
assert the process exits 75, and assert the emitted shell function delegates.

## 14.6 What must not regress

The 20 behaviors verified by mutation testing across three passes stay verified. The seats
identified these as directly at risk from this refactor and they need explicit attention:
file mode 0600 / directory 0700 (#9), the five-minute refresh margin (#10), atomic
temp-plus-rename (#17), a deletion never resurrected by a concurrent insert (#18), `clear`
removing a legacy `.cache.lock` (#19), and the TestMain real-config guard (#20).

## 14.7 Why this satisfies the stability criteria

- **L1 structural, not conventional** — the compiler, not discipline, prevents the bypass.
  Demonstrated, not asserted: a deliberate violation written in package main
  (`cache.diskCache{}`) is rejected at compile time with "name diskCache not exported by
  package cache".
- **L2 defect classes impossible** — an unlocked whole-cache write cannot be expressed
  from outside the package.
- **L3 NOT met — stated honestly.** Mechanism did leave the read path: `Get` sheds the
  lock and, with it, a timeout, an exit-status mapping, and a fallback branch. `main.go`
  drops 892 → 728 lines and retains zero cache primitives.

  But total production code RISES 892 → 956 lines (+64: `main.go` 728 plus
  `internal/cache/cache.go` 228), because the package boundary adds a constructor, type
  declarations, and method signatures. L3 asked for the mechanism count to go down. It did
  not. What happened is a trade, and it should be read as one: **64 lines of boilerplate
  buy a compiler-enforced invariant.**

  That trade is worth making — an invariant the compiler enforces is categorically
  stronger than one five review rounds enforced by hand — but calling it a reduction would
  be false. Any future claim that this refactor "simplified" the code is wrong; it
  relocated enforcement and paid boilerplate for the privilege.
- **L4 predictable under adversity** — flock is released by the kernel on process death;
  writes stay bounded; no manual recovery step exists or is needed.
- **L5 small surface** — no new dependency; the exported API is `New` plus four methods.

## 14.8 Known residual items (not blocking; recorded rather than hidden)

Three independent reviewers cleared this code with zero rejects and zero regressions. The
following remain open by decision, not by oversight.

**Legacy unnormalized cache rows become unreachable.** `Put` now normalizes before writing
(14.2), symmetric with `Get` and `Invalidate`. All three reviewers independently confirmed
this is correct and that ordinary records written by earlier builds — already normalized
upstream by the CLI — are read and superseded correctly. The exception is a record whose
stored host/owner/repo contains whitespace or a `.git` suffix: it can no longer be matched
and becomes a dead row. The consequence is one wasted re-mint plus a stale entry on disk,
never a wrong credential. `clear` removes it. Cleanup on read was judged optional hardening
rather than a fix.

**The MkdirAll/Chmod window.** `os.MkdirAll(s.dir, 0700)` is immediately followed by
`os.Chmod(s.dir, 0700)`, so the observable end state is always 0700 and mutating the
MkdirAll constant alone changes no test outcome. Two reviewers judged this benign: during
the window the directory is empty, is not writable by other users, and is restored to 0700
before any lock or credential file is created. One reviewer disagreed, holding that fresh
creation opens a real if narrow traversal window. Recorded as a genuine 2-1 split rather
than resolved by majority.

**Shell-wrapped `gh` cannot invalidate a rejected token before its refresh margin.** This
is a contract limit, not a defect: the wrapper cannot observe `gh`'s HTTP status. Git gets
invalidation through the credential helper's `erase`. A user hitting this runs
`gh-app clear`. Documented in section 6 and in the README.

---

# 15. Credential identity and private-key permissions (2026-08-06)

## 15.1 Cache entries must name a currently configured App

Cache lookup is exposed as `GetForApps(target, configuredAppIDs)`, so an external caller
cannot request an entry without stating the current App identities. An entry whose
`app_id` is absent is a read-only miss; the normal probe-and-mint path replaces it through
`Put`. Lookup does not delete or mutate rows and remains lock-free.

Known limit: membership does not detect replacement of a private key under an unchanged
`app_id`; this is a deliberate choice rather than an oversight. The token model suggests
that rotation or revocation does not invalidate an already-issued installation token: the
private key signs the App JWT used to request the token, while the issued token is an
independent opaque bearer credential with its own expiry and is not subsequently verified
against that key. This run did not verify a documented GitHub guarantee or confirm
GitHub's server-side behavior on key revocation. For key rotation or suspected compromise,
run `gh-app clear` and revoke the credential server-side on GitHub; no local cache
mechanism can revoke a token an attacker already holds.

## 15.2 Refuse exposed private keys at the signing boundary

`makeJWT` opens the private-key path, stats that open descriptor, requires a regular file
with no group or other permission bits, and reads from the same validated descriptor.
Modes such as `0600` and `0400` are accepted; `0644` is refused with the actual mode and
a `chmod 600` remedy. The tool never changes permissions on the user's key.
