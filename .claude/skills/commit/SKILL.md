# Skill: commit

Create a commit message matching the style of this repository and stage the commit.

## Process

1. Run `git status` (without `-uall`), `git diff --cached`, and `git log --oneline -5` in parallel.
2. Run `make fmt` to format code before staging.
3. If there are no staged changes, stage the relevant modified files (prefer specific files
   over `git add -A`). Never stage `.claude/` directory files, `.env`, credentials, keys,
   markdown docs, or plan files.
4. Draft a commit message following the format below.
5. Show the proposed commit message to the user and wait for approval before committing.
6. Write the commit message to a temp file, then commit with `git commit -F /tmp/commit-msg.txt`.
   Using `-F` avoids shell quoting issues with apostrophes and special characters that break
   HEREDOC constructs. Do NOT add `Co-Authored-By` lines unless for a real human co-author.
7. Run `git status` after the commit to verify success.
8. Clean up: `rm -f /tmp/commit-msg.txt`.

## Subject Line

```
area: short imperative description
```

This repository is public. Do not add internal ticket keys (Jira or similar), internal links,
internal system names, or customer names to the subject or body. Reference GitHub issues with
`Fixes #123` or `See #456` instead. Keep the subject to 72 characters or fewer, lowercase
after the area prefix, no period at the end. See the Contributing section of `README.md`.

Area tokens are derived from the primary changed file paths:

| Path prefix | Token |
|-------------|-------|
| `controllers/nodectrl/` | `nodectrl` |
| `controllers/betaclusterctrl/` | `betaclusterctrl` |
| `controllers/tenantctrl/` | `tenantctrl` |
| `controllers/clusterctrl/` | `clusterctrl` |
| `controllers/migrationctrl/` | `migrationctrl` |
| `controllers/` (cross-cutting) | `controllers` |
| `manifests/` | `manifests` |
| `kube/` | `kube` or `kubectl` |
| `manager/` | `manager` or `operator` |
| `api/` | `api` |
| `pkg/` | `pkg` or the specific sub-package |
| `scripts/` | `scripts` |
| `build/` | `build` |

When changes span multiple areas pick the one that drives the behavior change. For small or
housekeeping commits the area token can be omitted entirely.

Examples:
- `migrationctrl: harden stop and rollback gap detection`
- `manager: skip conversion webhook when no v1alpha1 objects exist`
- `nodectrl: fix pod hash excluding wrong container`
- `manifests: update golden files for TLS flag change`

## Body

Separated from title by a blank line. Explain **what** changed and **why**, not how (the
diff shows how). Use present tense for describing the new behavior. Use "Previously, ..." to
describe what was wrong or different before. Write in paragraph form, not bullet points. Wrap
lines at ~72 characters. Use an informal tone matching the existing history. Do not repeat the
subject line in the body. Do not use "This PR" — say "This commit" or just describe the
change directly.

**Keep it proportional**: Most commits need only 1-2 sentences. Simple additions or refactors
should be brief. Only large architectural changes warrant multiple paragraphs.

**Avoid spam**:
- Don't list every file changed or every helper function added
- Don't describe implementation details like "Update MockExecutor", "Add tests", "Add wrapper
  type" — the diff shows these
- Don't create laundry lists of minor changes
- Don't mention how tests were modified unless the commit is exclusively about tests
- If you can't explain why the change matters in 1-2 sentences, the title may be all you need

This repo does not use CRDB-style release notes.

### Full Example

```
migrationctrl: harden stop and rollback gap detection

Previously, restoreGapNodeOnStop swallowed STS update failures and
persisted PhaseStopped even when the cluster was one node short.
Rollback also ran health checks unconditionally, blocking progress
when the target CrdbNode never existed.

restoreGapNodeOnStop now returns an error so the caller requeues
instead of finalizing a degraded stop. Rollback skips health checks
for missing CrdbNodes since restoring them can only improve health.
syncMigratedNodesStatus compares by set equality instead of length.
```

## Workflow

1. Run `/review` on the staged changes to catch any issues before writing the message.
   Address anything it flags first.
2. Run `git diff --staged` and `git status` to understand the scope of what changed.
   Check that no `.claude/` files are staged — that directory is gitignored for a reason and
   must never be committed. If `.claude/` files appear in the staging area, unstage them
   and flag this to the user before proceeding.
3. Infer the area token from the file paths in the diff.
4. If there is a related GitHub issue, reference it in the body. Never add an internal ticket key.
5. Check for an open or merged PR on this branch:
   `gh pr list --head $(git branch --show-current) --state all --limit 1`
6. Draft the subject and body, then show the complete message for review.
7. Ask for confirmation, then run `git commit -F /tmp/commit-msg.txt`. Group all logical
   changes into one commit rather than splitting across many small ones.
8. After committing, verify with `git log --oneline -1` that the commit recorded correctly
   and the subject matches what was shown to the user.
