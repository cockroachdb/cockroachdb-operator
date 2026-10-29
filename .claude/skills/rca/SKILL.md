# Skill: rca

Write a customer-facing Root Cause Analysis document for an operator incident. Follows the
format established in existing RCAs in this repo.

If you have logs, events, or resource specs from the incident, run `/investigate` first to
analyze the reconciliation behavior and identify the root cause before drafting here. The
investigation output feeds directly into the Root Cause Analysis section.

## Usage

```
/rca
```

Then provide any combination of: incident description, customer report, support ticket link,
operator logs, kubectl describe output, or an already-run `/investigate` result.

---

## Information to gather before writing

Ask the user for the following. Do not draft the RCA until you have at least the first four.

1. **When did it start?** Approximate date and time, timezone, and whether the onset was
   sudden or gradual.
2. **What did the customer observe?** The symptom as the customer described it — not your
   interpretation of it yet.
3. **What was the actual impact?** Was there data loss? Service disruption? Which component
   failed? How many pods/nodes were affected? Did the database remain accessible?
4. **What was the root cause?** If known, explain it. If not fully known, run `/investigate`
   with the available logs and events first.
5. **What was the fix?** What changed in the code or image, and in which operator version /
   Helm chart version was it released?
6. **What remains outstanding?** Anything the fix does not address, deferred follow-up items,
   or known remaining noise.
7. **Verification steps.** What `kubectl` commands can a customer run to confirm the fix is
   working on their cluster?
8. **Support ticket link**, if one exists.

If the root cause is not yet clear, run `/investigate` with the available evidence and come
back with the findings.

---

## Document format

Write the RCA as a Markdown file. Name it: `RCA_<short-description>.md`

Keep the tone professional and customer-facing throughout. Use past tense for what happened
and present tense for what is fixed. Never use internal jargon without explanation. State
clearly whether data was lost or service was disrupted — customers read for this first.

```markdown
# Root Cause Analysis: [Concise title describing the failure]

## Summary

[One paragraph. Cover: when the issue started, what the customer observed, the root cause in
one sentence, what the fix does, and whether data loss or service disruption occurred. This
paragraph is what most customers read and stop at — make it complete and accurate.]

## Incident Description

**Symptom:** [What the customer observed. The external-facing description, not the internal
cause. Be specific: which component, which time, what the error looked like.]

**Why This Happened:** [One or two sentences explaining the triggering condition in plain
language. Keep this at the customer level — save the technical depth for the RCA section.]

**Observation:** [Bullet or prose list of what was observable during the incident:
- Which resources were in a bad state
- What logs showed
- What was stable (pods not restarting, database accessible, etc.)
- Any misleading signals (e.g., container appeared healthy but was silently failing)]

**Impact:** [What broke and for whom. State explicitly: was there data loss, service
disruption, or connection failures? If none, say so. How many nodes or clusters were
affected? Was manual intervention required?]

## Root Cause Analysis

[Narrative explanation of the technical root cause. Use sub-sections if the cause has
distinct layers. Common sub-sections:]

### Background

[Context needed to understand why the component exists and what it is supposed to do.
Only include this if it is not obvious to the reader.]

### How the Issue Was Introduced

[What code path, configuration change, image migration, or environmental condition caused
the bug to exist. Be specific: which package was missing, which condition was evaluated
incorrectly, which assumption was violated.]

### Why the Issue Was Silent

[If the failure mode was hard to detect, explain why. What masked the problem? What
would have needed to happen for it to be caught earlier? This builds trust by showing
you understand the detection gap, not just the cause.]

## Resolution

[One or two sentences describing what changed. Then use sub-sections below.]

### Fix released [date] ([Helm chart version / operator image SHA])

[Brief description of what the fix does technically.]

### What This Fix Resolves

- [Capability or failure mode that is now working correctly]
- [Second capability]
- [Third — e.g., silent failure prevention, startup validation, etc.]

### What Remains Outstanding

- [Anything the fix does not address. Be honest — partial transparency builds more trust
  than claiming everything is fixed.]
- [Deferred follow-up items with approximate timing if known]

[If nothing remains outstanding, omit this sub-section.]

## Action Items

### Update Operator

**Immediate Action Required:** [State clearly who should upgrade and why.]

**To check your version:**
```bash
# For Helm installations
helm list -n <namespace>
# Look for chart version [fixed-version] or later
```

**Affected versions:** [Any operator version released before <date>.]

**Fixed in:** Helm chart version **[version]** ([date]) and later.

**Helm chart reference:** [link]

### Verification Steps

After upgrading, verify the fix is working:

1. [Step with kubectl command]
   ```bash
   kubectl exec -it <pod-name> -c <container> -- <command>
   # Expected output: [what success looks like]
   ```

2. [Next verification step]

3. [Check for the original symptom being absent]
   ```bash
   kubectl logs <pod-name> -c <container> | grep -i "<error-string>"
   # If you see this error, you are running an affected version
   ```

## Related Information

- **Fix release date:** [date]
- **Fixed Helm chart version:** [version] and later
- **Helm chart repository:** [link]
- **Affected component:** [component name]
- **Root cause:** [one-line summary]
- **Support ticket:** [link if available]

## Contact

For questions or concerns regarding this issue, please contact the CockroachDB support team.
```

---

## Quality checks before finalising

Before handing the draft to the user, verify:

- The Summary paragraph is self-contained — a customer who reads only that paragraph
  understands what happened, why, and what to do.
- Data loss and service disruption status are stated explicitly, not implied.
- Every `kubectl` command in Action Items has a comment showing the expected output.
- "What Remains Outstanding" is honest — do not omit it just because the list is uncomfortable.
- The "Why the Issue Was Silent" section is present if the failure mode was non-obvious. This
  is the section customers and their managers care most about after a surprise incident.
- Version numbers, dates, and image SHAs are accurate — ask the user to confirm these
  before finalising.
- Operator-internal terms (CrdbNode, reconciler, resourceVersion) are explained on first use.

---

## Converting to a Google Doc

If the user wants a Google Doc, use the `/gdoc` skill after the Markdown is approved. Apply
bold formatting to the four Incident Description labels (Symptom, Why This Happened,
Observation, Impact) and to the Action Items sub-headings (Update Operator, Verification
Steps). Code blocks become monospace paragraphs. Everything else maps directly.
