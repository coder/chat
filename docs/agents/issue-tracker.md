# Issue tracker: GitHub Issues

The public source of truth for this repo's roadmap, feature requests, and bug
reports is GitHub issues:

https://github.com/coder/chat/issues

Anything user-facing — planned work, accepted/rejected proposals, bug state —
belongs there. Use `gh issue` to read and update it.

## `.scratch/` is internal working notes only

The `.scratch/` directory holds internal working notes: PRDs, implementation
breakdowns, and drafts that agents produce while working. It is not a public
roadmap, it makes no promises, and nothing in it should be treated as
authoritative over GitHub issues. When a scratch note graduates into real
planned work, file a GitHub issue for it.

### Conventions for `.scratch/`

- One feature per directory: `.scratch/<feature-slug>/`
- The PRD is `.scratch/<feature-slug>/PRD.md`
- Implementation notes are `.scratch/<feature-slug>/issues/<NN>-<slug>.md`, numbered from `01`
- Triage state is recorded as a `Status:` line near the top of each file (see `triage-labels.md` for the role strings)
- Comments and conversation history append to the bottom of the file under a `## Comments` heading

## When a skill says "publish to the issue tracker"

Create a GitHub issue with `gh issue create`. Use `.scratch/<feature-slug>/`
only for supporting working notes that are not ready to be public.

## When a skill says "fetch the relevant ticket"

If the reference is a number or URL, read the GitHub issue with
`gh issue view`. If the reference is a path, read the file at that path.
