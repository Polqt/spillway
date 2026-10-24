# CLAUDE.md - Spillway

@AGENTS.md

Everything above, imported from `AGENTS.md`, applies to Claude Code as well.
This file adds only the Claude Code specific parts.

## Agents and skills

| Workflow step | Use |
|---|---|
| Sharpen an idea | `/grill-with-docs` |
| 1. Plan | `planner` agent |
| 2. Ticket | `/to-tickets` on the plan |
| 3. Implement | `/implement` per ticket (drives `/tdd`), `/clear` between tickets |
| 4. Verify | `verifier` agent |
| 5. Review | `/code-review` since the milestone's starting commit |
| 6. Security audit | `security-auditor` agent |
| 7. Checklist | Reopen the ticket after verification, tests, fixes, review and any required security audit. Check only acceptance items supported by evidence, record unmet items, and report completion only when all are met. |
| Feature branches and pull requests | Follow the branch, rebase, and PR standards in `AGENTS.md` |
| Hard bug | `/diagnosing-bugs` |

Project agents live in `.claude/agents/`; the slash skills are global.
Run `verifier`, `/code-review` and `security-auditor` in subagents so they review with fresh eyes, not the context that wrote the code.
Fix every Critical or High security finding and every review finding that breaks a hard rule or the OpenAI contract, then re-run `verifier`.

## Senior software engineering

Apply the KISS, DRY, YAGNI, clean-code, engineering-judgment, and evidence rules in `AGENTS.md` to every change.
Prefer a small, cohesive solution that preserves established contracts, avoids speculative complexity, and can be verified through user-visible behavior.
For Go, treat each directory as a package boundary and name it for one cohesive responsibility.
Keep CLI subpackages small, keep dependencies pointed toward domain packages, and do not create folders solely to file commands or reduce the number of source files.

## Release documentation

When publishing a release tag, update the current-version install instructions in `README.md` and `docs/install.md` to that published tag.
Remove outdated release-candidate status wording after the stable tag is published.

A hook in `.claude/settings.json` runs `gofmt -w` on every edited `.go` file and reports syntax errors back.

## Agent skills

### Issue tracker

Issues live as local markdown files under `.scratch/<feature>/`. See `docs/agents/issue-tracker.md`.

### Triage labels

Default five roles (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`), recorded as a `Status:` line in each issue file. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.
