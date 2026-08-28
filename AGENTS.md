# AGENTS.md

## Commits

Every commit message must follow [Semantic Commit Messages](https://gist.github.com/joshbuchea/6f47e86d2510bce28f8e7f42ae84c716).
This holds for the whole history, so a commit that does not follow it gets reworded rather than left alone.

```
<type>(<scope>): <subject>
```

The scope is optional, and in this repository it is a directory or subsystem: `nix`, `config`, `probe`, `telemetry`.

Use these types, and pick by what the change does for the user rather than by which files it touches:

- `feat`: a new feature for the user, not a build script addition.
- `fix`: a bug fix for the user, not a build script fix.
- `docs`: a change to documentation.
  A README-only change is `docs`, never `chore`.
- `style`: formatting, missing semicolons, and the like, with no change to production code.
- `refactor`: production code that neither fixes a bug nor adds a feature.
- `test`: adding or reworking tests, with no change to production code.
- `chore`: dependency bumps, build tooling, and other routine work, with no change to production code.

Write the subject in the present tense, as an instruction to the codebase: "add the reload timer", not "added" or "reload timer".
Keep it under 72 characters, start it lowercase, and end it without a full stop.

The body is optional and separated by a blank line.
State why the change is needed and what it does differently, because the diff already states what changed.
The prose rules below apply to the body too, so one sentence per line.

The `commits` check on every pull request runs [commitlint](https://commitlint.js.org) against the rules in `.commitlintrc.yml`.
Run it over your branch before you push:

```
npx -y @commitlint/cli --from origin/main --to HEAD --verbose
```

Two of the rules above are not machine-checkable, so they are on you.
No linter can tell that a README-only change was typed as `chore` instead of `docs`, and none can tell present tense from a noun phrase: `feat: systemd unit health endpoint over HTTPS` passes commitlint and is still wrong.

## Dependencies

[Renovate](https://docs.renovatebot.com) opens the update pull requests, configured in `renovate.json`.
It is used rather than Dependabot for one reason that decides it: Renovate updates `flake.lock`, and Dependabot has no Nix support at all, so half this repository's dependencies would go unwatched.

Renovate also runs `go mod tidy` and `go mod vendor` after a Go bump, which the `vendor` CI job requires and which a hand-edited `go.mod` would fail.
Its commits are `chore(deps): ...`, which the commit check accepts.

> [!NOTE]
> A Renovate subject longer than 72 characters fails the commit check on Renovate's own pull request.
> Reword that one commit by hand, or lower `header-max-length` to a warning if it turns out to happen often.

## Documentation

@README.md is the only user-facing document.
It is the GitHub landing page, the configuration reference, and the NixOS install guide at once, so keep it complete and keep it short.
There is no separate command reference to sync with.

When you change behavior that a user can observe — a config key, a default, a metric name, an HTTP status, a log field, a NixOS module option — update @README.md in the same change.

### Keep examples true

Every example in @README.md must be copied from something that actually works, not written from memory.

- YAML config examples must use the keys that `internal/config` parses.
  Parsing is strict, so an invented key in the README is a config that refuses to start.
- Nix examples must use options that `packaging/nix` declares, which the flake exposes as `nixosModules.default`.
- The metrics table must list the instrument names, types, units, and attributes that the code records.
  Prefer [OpenTelemetry Semantic Convention](https://opentelemetry.io/docs/specs/semconv/) names where one exists, and say so when a name is project-specific.
- Command output shown in a code block must be real output, trimmed.
  When the output is longer than 4 lines, truncate it meaningfully to 4 lines or less.

### Validation of changes

When you change output that users see, build the program and check the output before you write about it:

```
nix flake check
```

## Prose rules

Follow these rules when writing or editing prose in this project.

### Line and paragraph structure

- **One sentence per line** (semantic line breaks).
  Each sentence starts on its own line; do not wrap mid-sentence.
- Separate paragraphs with a single blank line.
- Keep paragraphs between 2 and 5 sentences.

### Voice

State the reason next to the fact.
A reader of this README is deciding whether to expose a health endpoint to the internet, so a rule without its rationale is a rule they will get wrong.
Write "the initial certificate load is fatal, because a listener that cannot complete a handshake is worse than one that is not up yet", not "the initial load is fatal".

Say what the program does not do.
No debouncing, no grace period, no maintenance window, no OTLP export of logs: the absent features are what stop someone from building on an assumption.

### Section headers

Write section headers in sentence case, e.g., "Running it on NixOS".

### Links

- Use inline Markdown links: `[visible text](url)`.
- Link the most specific relevant term, not a generic phrase like "click here" or "this page".
- Link the upstream specification the first time you name it, e.g. [OpenTelemetry declarative configuration](https://github.com/open-telemetry/opentelemetry-configuration).

### Code blocks

- Fence with triple backticks and a language identifier (e.g., ` ```yaml `, ` ```nix `).
- Use code blocks to provide illustrative examples.
- **One independent command per code block.**
  Do not stack unrelated commands inside the same ` ```bash ` block.
  A reader's "copy" action should never grab more than one thing they intended to run.
  Exceptions: a multi-line invocation continued with `\`, a `key=value` env-var prefix followed by the command, or a pipeline (`curl ... | jq`) — those are a *single* command.
  Workflows that genuinely involve several steps use one code block per step, with prose between them describing what the previous step accomplished and what the next one does.
- A YAML or Nix example is a whole configuration, so one block is correct even when it sets many keys.
  Use an end-of-line comment to give a default or a unit, e.g. `listen: ":443"  # default ":8443"`.

### Punctuation and typography

- End sentences with full stops.
- Use the **Oxford comma** (e.g., "the certificate, the key, and the token file").
- Use **straight quotes** everywhere, in prose and in code blocks alike.
  This differs from the dash0-cli convention on purpose: this README is read as raw Markdown on GitHub and copied into shells and Nix files, where a curly quote is a bug.
- Write numbers as digits and spell out "percent" (e.g., "10 percent", not "10%" or "ten percent").
- Keep durations and sizes in the syntax the config accepts: `30s`, `3s`, `0640`, `:8443`.

### Callouts

Use a GitHub alert for a caution that would otherwise interrupt the paragraph: `> [!NOTE]`, `> [!TIP]`, `> [!IMPORTANT]`, or `> [!WARNING]`.
Reserve them for the cases where the program looks healthy and is not — a stuck process that holds its main PID, a missing propagator that produces orphan spans.
An alert that states something ordinary spends the attention the next one needs.

### Naming things

- Write the program name as `systemd-unit-healthz` in code formatting when you mean the binary or the NixOS module, and as plain text when you mean the project.
- Treat systemd, D-Bus, and OpenTelemetry names as code: `ActiveState`, `SubState`, `RemainAfterExit`, `Restart=always`, `org.freedesktop.systemd1.Manager`, `traceparent`.
- Write an abbreviation in full at first use, then use the abbreviation.
- Use American English.
