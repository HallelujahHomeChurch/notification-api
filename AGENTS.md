# Repository Instructions

## Delivery Policy

- Treat `main` as the production branch. Do not commit or push directly to `main`.
- Create a focused branch for every task and submit changes through a pull request.
- CI must pass before merge. Do not bypass, ignore, or merge around a failing required check.
- Keep changes small and reviewable, and use squash merge unless the repository explicitly requires another strategy.
- For deployable applications and services, a merge to `main` is the release boundary: build an immutable artifact, deploy it through CI/CD, then verify health and the production route.
- Never deploy an unmerged local commit or replace the CI/CD path with an ad hoc production command.
- If deployment fails, preserve the last healthy revision, report the failure, and use the repository rollback path.

## Multi-repository Codex tasks

- First read the checked-out `hhc-planning/AGENTS.md` and
  `hhc-planning/docs/cloud-environments.md` at the planning revision recorded in
  the task. Sibling repositories do not inherit those instructions automatically.
- This repository owns its setup and verification: read its README, runtime
  manifests and CI before choosing commands. Report missing dependencies or
  unverified Linux startup instead of claiming a successful run.
- Preserve unrelated edits and active writer ownership. Use an isolated task
  branch and follow the task's explicit review, merge and deployment authorization.
- Pure human documentation uses lightweight validation. Source, workflow, runtime
  configuration, executable skills and mixed changes retain full verification.
  Record read-only inspection, test execution, CI and live acceptance separately.
