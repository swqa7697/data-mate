---
name: release-pr
description: Open or locate the Data Mate release PR into main after a pushed version bump. Use for "release PR", "open the release", or "PR for the bump"; validates remote release files and prepares a changelog-based PR with gh.
---

Open `main <- <current branch>` for Data Mate. Use remote refs as the source of
truth. Never stage, commit, push, switch branches, merge, or tag as part of
`release-pr`. Those actions require separate explicit user authorization.
Codex uses `$release-pr` from the canonical `.agents/skills/release-pr` directory.
Claude Code uses `/release-pr` through the `.claude/skills/release-pr` symlink.

Run from the repository root with Git and authenticated `gh`.

1. Read `AGENTS.md` and the README release instructions. Fetch `origin --prune`.
   Resolve the current branch; stop on detached HEAD, `main`, or `master`.
   Resolve `origin/<branch>` and `origin/main`; stop if either is missing.
   Quote every branch/ref argument in commands.
2. Read the subject and `VERSION` at the remote branch tip. Require exactly
   `release data-mate: X.Y.Z`, where the stable version has no leading
   zeroes and equals that remote `VERSION`. Require a version increase over
   `origin/main:VERSION` and one nonempty matching dated CHANGELOG section.
   Follow the version/changelog contracts in `internal/devtools/release`.
   Stop with the failed prerequisite if validation fails; do not repair Git state.
3. Check `git rev-list --count origin/main..origin/<branch>`; stop if zero.
   Use `gh pr list --repo swqa7697/data-mate --base main --head <branch>
   --state open --json number,url`. If a PR already exists, report its URL
   without creating or editing another PR.
4. Read the remote version comparison, matching CHANGELOG section, non-merge
   commit log, and diff against `origin/main`. Inspect command, configuration,
   distribution, dependency, and compatibility changes relevant to the release.
   Use changelog entries as the release-note source; describe final user-visible
   behavior rather than intermediate development history.
5. Use the release commit subject verbatim as the PR title. Write a concise body
   with the old/new version, user-facing highlights, applicable changelog
   categories, compatibility/setup changes, and actual validation results or
   clearly identified unverified checks. Include this release checklist:
   - Merge after required CI checks pass.
   - Update local `main`, then have the operator run `make tag` and complete its
     CAPTCHA. The tag push starts signing, fresh-runner acceptance and publication.
   - Keep immutable releases enabled and the `release` environment restricted
     to `v*` tags, with the four documented Apple signing secrets configured.
   - Check the hosted release run and published assets. Record outstanding
     interactive Keychain, agent, and genuinely toolchain-free host checks.
   Never claim planned checks passed. Do not run `make release-commit` or `make tag`.
6. Write the exact body to a disposable file in `/tmp`, then run
   `gh pr create --repo swqa7697/data-mate --base main --head <branch>
   --title <subject> --body-file <path>`. Remove the temporary body after use.
   Report the PR URL, version change, and remaining release prerequisites.

Do not request additional approval to create the PR when the user has already
asked `release-pr` to open it. Failed GitHub requests stop this invocation;
inspect whether a PR was created before retrying to avoid duplicates.
