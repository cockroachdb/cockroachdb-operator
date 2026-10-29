# Commenting Standards

These conventions apply to all Go code in this repository. They reflect the informal style
of the existing codebase rather than formal documentation standards.

## Default to fewer comments

Most lines need no comment. Bias toward deleting them — a comment that restates
the code, narrates the change, or repeats purpose stated elsewhere is worse than
none. Match the surrounding file's (usually sparse) density. Comments earn their
place by explaining *why*: a non-obvious choice, a subtle ordering, a workaround,
an invariant.

- **Large block comments** belong at the struct, type, or component level,
  stating purpose, lifecycle, and invariants once.
- **Functions** — especially private ones — get short comments: one sentence on
  the contract, or none if the signature says it. Don't re-document what lives on
  the type. Reserve worked examples for contracts hard to grasp without one.

## Block comments

Block comments are full sentences: capitalized first word, period at the end. Place them
directly above the declaration or block they describe with no blank line in between. For
exported identifiers the comment must start with the identifier name.

```go
// NodeReconciler reconciles CrdbNode resources and drives the pod lifecycle state machine.
type NodeReconciler struct { ... }
```

## Inline comments

Inline comments are lowercase with no terminal punctuation. Use them sparingly and only when
the intent is not obvious from the code itself. Write a plain sentence fragment that adds
context the code cannot convey on its own. Do not use em-dashes, colon-prefixed labels, or
ALL-CAPS words to front-load emphasis. If something is important enough to call out, make the
code itself clear and let the comment explain why in plain prose.

## Tone and style

Write as a developer leaving a note for the next person reading the code. Be direct and
confident — say what the code does and why, not "this might be" or "we probably need to."
Keep sentences short and simple. One idea per sentence is usually enough.

Match the informality of surrounding comments. If nearby comments read like casual prose,
yours should too. Avoid sounding like documentation: no formal definitions, no exhaustive
enumerations, no passive voice when active works just as well.

Comments should read like a developer talking to the next person who will read the code.
The codebase uses a variety of natural openers — not just "we":

- "We" for intent: "We skip nodes that are mid-update because..."
- Reasoning out loud: "We're not at our desired scale yet, but we've reached the limit..."
- Defining terms inline: "Active in this case means nodes not yet decommissioning."
- Confident observations: "Unknown shouldn't happen. According to the docs, it hasn't been used since 2015."
- Conditional intent: "If the pod is not running, we should delete it immediately instead of waiting."
- Side notes: "NB: Currently, the cluster controller will delete CrdbNodes..."

Vary the opener naturally based on what you're explaining. Avoid always defaulting to "we"
just as much as avoid always defaulting to passive voice. The goal is to explain the *why*
in a tone that fits the surrounding code.

The best reference for the correct tone is the existing inline comments in
`controllers/betaclusterctrl/update.go`. These are the canonical examples of what
comments in this codebase should sound like:

```go
// Skip all CrdbNodes that are on an out of date revision. Any previous
// revision could be unstable, "bad", or contain a bug that leads to
// unreadiness. We don't want to let those cases prevent us from
// rolling out a new update. Our PDB will prevent us from taking down
// more than our given allowance of Pods.

// If the k8s node of a pod has changed, that implies the old node
// doesn't exist or is unschedulable.

// At this point we know that the CrdbNode and its .Status field are up
// to date. If the nodectrl thinks the Pod needs to be updated, we are
// not going to let the Pod's unreadiness prevent a rolling update from
// occurring as there may be an issue with currently running Pod.
// The combination of the PDB and eviction helper will ensure that any
// updates are paced appropriately.

// If the pod is on the current revision but not yet running or ready, the
// reason matters. We handle a few distinct situations differently here.

// A pod with no NodeID has never joined the cluster. If it's pending
// during an active upgrade, old pods probably still carry pre-upgrade
// affinity rules that prevent this pod from scheduling. We let the
// rollout continue so old pods update first and the new rules take effect.

// If the pod is running but not ready, the new spec may be bad. We don't
// want to roll it to more pods, so we disable evictions and wait for this
// pod to recover before allowing the rollout to continue.
```

Notice: short sentences, natural varied openers (Skip/If/At this point/A pod with...),
and each comment explains *why* the code does what it does, not just *what* it does.
A comment that only restates the condition in prose adds no value.

## What to avoid

Do not add parenthetical phrases like "(e.g., ...)" unless they genuinely add something that
cannot be said directly. Do not use em-dashes as connectors inside comments. Do not use bold
or capitalized keywords like IMPORTANT or NOTE as prefixes. Keep the tone informal and match
the style of surrounding code rather than trying to be comprehensive.

Avoid using punctuation as structural labels inside comments. Colons after a word to introduce
a clause (`reason: because...`), semicolons joining unrelated thoughts, and curly braces or
brackets as visual grouping all signal over-engineered prose. If you need that much structure,
the comment is doing too much work — split the logic instead.

## Numbered lists and multi-block comments

A single prose paragraph is not always clearer than a list. When a function handles several
distinct scenarios or has non-obvious branches, a numbered list is fine. Each item is written
as a plain sentence or sentence fragment — no label-colon prefix, no em-dash connector, no
semicolons joining unrelated thoughts.

```go
// Wrong — label-colon and semicolons:
//  1. Pod not yet created: applies spec immediately.
//  2. Pod mid-update: leaves it alone; nodectrl will complete the eviction.

// Right — plain sentences, no structural punctuation:
//  1. If the pod doesn't exist yet, apply the spec immediately.
//  2. If the pod is mid-update, leave it alone. Nodectrl will complete the eviction.
```

When a single comment block would become a wall of text, use blank `//` lines to separate
distinct thoughts, or split into multiple shorter blocks placed directly above the relevant
code. Each block or paragraph should be self-contained. Do not repeat context across blocks.
Each block adds new information and follows the same style rules as any other comment.

Use blank `//` lines between numbered items that each need more than one line of explanation,
or between a preamble sentence and the list that follows it. A blank line signals a topic
boundary without introducing structural labels.

```go
// applyFoo handles pods that need an immediate spec update.
//
//  1. If the pod does not exist yet, apply the spec so nodectrl can create one.
//
//  2. If the pod is on an unknown revision, update it now. Unknown revisions
//     are unreliable and should not be allowed to persist.
//
//  3. If the pod is mid-update, leave it alone. Nodectrl will complete the eviction.
```

Specific patterns that violate these rules — all of these have been found in real PRs:

- `// Guard: ...` → remove the label, write the condition plainly
- `// Strategy 1: ...` → drop the label; the comment should stand on its own
- `// Note: Do NOT ...` → drop `Note:`, lowercase `not`; use two sentences if needed
- `// For Helm: ...` / `// For Operator: ...` → use a sentence instead of a label
- `// field: because X` → rewrite as prose
- Inline `NEW` / `NOT` in all-caps → forbidden; lowercase and rephrase
- `// Note:` and `// Important:` are acceptable when the information is genuinely easy to
  miss or has a non-obvious consequence. Use them sparingly. When in doubt, rewrite the
  surrounding code or comment so the note is unnecessary.

Hyphens as joiners between words are fine when the compound reads naturally (`non-obvious`,
`pre-allocated`). Using a hyphen as a dash to connect two separate thoughts (`do X - then Y`)
is not. Use two sentences instead.

## When to add comments

Add a comment when you discover knowledge that was not documented. When a state machine
transition is non-obvious, explain which state you are leaving and why this path is taken.
When a field is excluded from a hash for a non-obvious reason, document the rationale at the
exclusion site rather than in a distant README. For example, `GetNodeHashRevisionV2` excludes
`ResourceRequirements` so that CPU and memory changes do not evict pods, enabling in-place
vertical scaling. When an error is intentionally demoted to a warning, say why.

## When to fix comments

Fix factually wrong comments immediately, the same way you would fix a bug. Update comments
whenever the behavior they describe changes.

## PR review suggestions

Prefix grammar-only or style-only suggestions with "nit:" so reviewers can tell them apart
from substantive feedback. Factual corrections in comments are not nits.

## Areas that need comments in this codebase

Reconciler state machine transitions need a comment on each branch explaining the condition
and the next state. Hash exclusions in `GetNodeHashRevisionV2` need a comment explaining why
each field is excluded and what that enables. Webhook validation blocks need a comment naming
the invariant being enforced. Any code path that splits on TLS vs insecure behavior should
note which branch it is in. Fixture files that were re-recorded for a non-obvious reason
should have a note explaining the trigger.
