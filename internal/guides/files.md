---
topic: files
title: Job and workspace files
description: Job-scoped vs workspace files, scripts next to jobs, If-Match.
---

# Job and workspace files

Supporting files for a job (scripts, templates, fixtures) go next to it. Use `trebi job file` — the default. Paths are job-relative (`render.py`, not `report/render.py`). A wrong prefix cannot touch a sibling job.

`JOB.md` / `MEMORY.md` writes on this command 409: edit those with `trebi job` and `trebi job memory`.

## Job-scoped files

```bash
trebi job file list --json --limit 100 <job>
trebi job file list --json --limit 100 <job> notes/
trebi job file get <job> render.py
trebi job file get <job> render.py --stat
trebi job file put <job> render.py --from ./render.py
trebi job file put <job> render.py --from ./render.py --if-match <hash>
trebi job file remove <job> render.py --if-match <hash>
trebi job file mkdir <job> fixtures
trebi job file mv <job> render.py lib/render.py
trebi job file mv <job> new.py lib/render.py --overwrite
trebi job file rmdir <job> fixtures --force
```

Folders are real. `mkdir` makes an empty folder, and a folder stays after you delete its last file. `mv` renames a file or a folder and makes the parent folders of the new path. A path that exists at the target returns 409 `conflict`; `--overwrite` replaces a file, never a folder. `rmdir` deletes a folder and all its files; ask the user first, then pass `--force`. Without it, the daemon names the count of files that the delete removes.

`--from` and `--out` are **local only** (local filesystem); remote: pass `content` on put, read the body on get. `--stat` returns JSON metadata (`path`, `size_bytes`, `mtime`, `hash`). `put` is a full replace. Re-read (`--stat`) after a 412 `precondition_failed`.

Stdin/stdout form (no local path flags):

```bash
cat render.py | trebi job file put <job> render.py
trebi job file get <job> render.py
```

`--workspace` disambiguates a duplicate job name, same as `trebi job show`.

## Workspace-level files

`trebi file` is for assets shared across jobs (`templates/`, `lib/`) or assistant support files. Same If-Match / size-cap rules. `JOB.md` / `MEMORY.md` are allowed there — the index rebuilds the same way as an editor save. `trebi.yaml` and `.git/**` are denied.

```bash
trebi file list --json --limit 100
trebi file list --json --limit 100 templates/
trebi file get templates/base.html
trebi file put templates/base.html --from ./base.html
trebi file remove templates/base.html --if-match <hash>
trebi file mkdir templates/email
trebi file mv templates/base.html templates/email/base.html
trebi file rmdir templates/old --force
```

`--from` is **local only**; remote: `content`.

Skill files use the same shape under `trebi skill file` — see `trebi guide skills-secrets-config`.
