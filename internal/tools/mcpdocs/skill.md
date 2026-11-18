# skill

Skill operations: manage reusable skill documents.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Scopes

A skill has one of four scopes. A run gets the skills of its job, its agent,
its workspace, and the global skills. When two skills have the same name, the
narrower one wins: job, then agent, then workspace, then global. The wider one
is "overwritten" for that run.

- `global`: all runs. The folder is `~/.trebi/skills/<name>`. Inherited skills from
  user harness skill dirs count as global.
- `workspace`: runs in one workspace. The folder is `<workspace>/skills/<name>`.
- `agent`: runs of one agent and of the jobs that it owns. The folder is
  `<workspace>/agents/<agent>/skills/<name>`.
- `job`: runs of one job. The folder is `<job folder>/skills/<name>`.

`owner` names the workspace, agent, or job that holds a skill, by name or ID.
`workspace` finds an agent or job name. A global skill has no owner.

When a name is in more than one scope and `scope` is not set, a read gets the
global skill and a write returns `ambiguous` with `details.refs`.

Who can write:
- A person and the assistant write all scopes. The assistant writes only global
  skills and the skills of its own workspace.
- An agent run writes only its own agent skills and the skills of jobs it owns.
- A job run writes only its own job skills.
- Any other write returns `forbidden`. To change a wider skill, create a copy of
  the same name in your own scope. The copy overwrites the wider skill for your
  runs.

## Actions

### list
List skills. Includes global skills from `~/.trebi/skills`, read-only
**inherited** skills discovered from user harness skill dirs and
`~/.agents/skills`, and the skills of every workspace, agent, and job. A
`SKILL.md` that is a single `@path` line is followed. Inherited rows carry
`provenance: inherited`, `origins[]` (tool, path, key), and `injected`. Each
row carries `ref`, `scope`, `owner`, `status` (`active` or `pending`),
`overrides` (the ref it overwrites), and `overridden_by` (the refs that
overwrite it).
- `scope` (string, optional): keep one scope.
- `owner` (string, optional): keep one owner; needs `scope`.
- `workspace` (string, optional): keep one workspace, or find an owner name.
- `effective_for` (string, optional): job or agent name or ID. The reply lists
  the skills that its runs get, narrow first. Each row has `used`, and
  `overwritten_by` when a narrower skill wins.
- `archived` (string, optional): `false` (the default) hides archived skills, `true` shows only archived skills, `all` shows both. Each archived row carries `archived_at`. The reply carries `counts` (`current`, `archived`).
- `limit` (number, optional): page size.

### show
Read one skill.
- `name` (string, required): skill name.
- `scope` (string, optional): scope of the skill. Use it when the name is in more than one scope.
- `owner` (string, optional): workspace, agent, or job that holds the skill, by name or ID.
- `workspace` (string, optional): workspace of an `owner` agent or job name.

### create
Create one skill. Give `content`, or one source field to pull the bundle.
- `name` (string, required): skill name.
- `content` (string, required unless a source field is set): skill document text.
- `scope` (string, optional): `global`, `workspace`, `agent`, or `job`. Empty is
  `global` for a person and the assistant. In an agent or job run, empty is the
  scope of that run.
- `owner` (string, required for a scope other than `global`): workspace, agent,
  or job name or ID.
- `workspace` (string, optional): workspace of an agent or job name.
- `note` (string, optional): at most 500 characters for the person who reviews
  the skill, for example a request to share it wider.
- `source_git` (string, optional): git remote to clone the skill bundle from. The clone must hold a `SKILL.md`.
- `source_folder` (string, optional): local folder to copy the skill bundle from. It must hold a `SKILL.md`.
- `provenance` (string, optional): origin of the skill, one of `local`,
  `assistant`, `git`, `registry`, `connector`. Leave it unset unless you know
  the skill came from elsewhere: it defaults to `local`, and only `local`,
  `assistant`, and `agent` skills accept support-file writes. A skill created
  in a run always gets `agent`. `inherited` is discovered from user home and
  cannot be created.

A skill that a run creates shows in the inbox. When the agent kit sets
`skill_policy.review`, the skill stays `pending` and runs do not get it until
`approve`. When the kit sets `skill_policy.create: false`, create returns
`forbidden`.

Mutations (`set`, `delete`, `refresh`, `move`, `file put`) on an inherited row
return `conflict`. A connector skill is managed: change the connection instead.

### set
Replace one skill document.
- `name` (string, required): skill name.
- `scope` (string, optional): scope of the skill. Use it when the name is in more than one scope.
- `owner` (string, optional): workspace, agent, or job that holds the skill, by name or ID.
- `workspace` (string, optional): workspace of an `owner` agent or job name.
- `content` (string, required): full skill document text.
- `harnesses` (object, optional): global skills only; harness name to skill path, for tools that keep their own copy.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### refresh
Pull the skill source again. The reply carries `updated`, which is false when
the source is unchanged, and false with a `note` when the skill has no source.
A skill made from a source keeps it, so `refresh` needs the name only.
- `name` (string, required): skill name.
- `scope` (string, optional): scope of the skill. Use it when the name is in more than one scope.
- `owner` (string, optional): workspace, agent, or job that holds the skill, by name or ID.
- `workspace` (string, optional): workspace of an `owner` agent or job name.

### move
Move one skill to another scope or owner: wider, narrower, or to another
agent or job. The caller must be able to write both scopes. The folder and its
files move with the skill.
- `name` (string, required): skill name.
- `scope` (string, optional): scope of the skill. Use it when the name is in more than one scope.
- `owner` (string, optional): workspace, agent, or job that holds the skill, by name or ID.
- `workspace` (string, optional): workspace of an `owner` agent or job name.
- `to_scope` (string, required): `global`, `workspace`, `agent`, or `job`.
- `to_owner` (string, required for a scope other than `global`): name or ID.
- `to_workspace` (string, optional): workspace of a `to_owner` agent or job name.
- `replace` (boolean, optional): delete the skill of the same name at the target.
- `rename` (string, optional): new name at the target.

A skill of the same name at the target returns `conflict` unless `replace` or
`rename` is set.

### approve
Make a pending skill active, so that runs get it. A run cannot approve.
- `name` (string, required): skill name.
- `scope` (string, optional): scope of the skill. Use it when the name is in more than one scope.
- `owner` (string, optional): workspace, agent, or job that holds the skill, by name or ID.
- `workspace` (string, optional): workspace of an `owner` agent or job name.

### archive
Archive one skill. An archived skill keeps its folder, but it leaves every run, the harness homes, and the assistant. A wider skill of the same name takes its place in the runs of its owner. The name stays in use: a new skill of the same name returns `conflict`. A global skill also loses its harness links; `unarchive` puts them back. A connector skill and an inherited skill return `conflict`. A refresh or an edit of the skill document keeps the archive. A change of `harnesses` on an archived skill returns `archived` (409). The reply is the skill with `archived_at`.
- `name` (string, required): skill name.
- `scope` (string, optional): scope of the skill. Use it when the name is in more than one scope.
- `owner` (string, optional): workspace, agent, or job that holds the skill, by name or ID.
- `workspace` (string, optional): workspace of an `owner` agent or job name.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### unarchive
Restore an archived skill. Runs get it again, and a global skill gets its harness links back. The reply is the skill.
- `name` (string, required): skill name.
- `scope` (string, optional): scope of the skill. Use it when the name is in more than one scope.
- `owner` (string, optional): workspace, agent, or job that holds the skill, by name or ID.
- `workspace` (string, optional): workspace of an `owner` agent or job name.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### delete
Remove one skill and its folder. Destructive.
- `name` (string, required): skill name.
- `scope` (string, optional): scope of the skill. Use it when the name is in more than one scope.
- `owner` (string, optional): workspace, agent, or job that holds the skill, by name or ID.
- `workspace` (string, optional): workspace of an `owner` agent or job name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

## Related guides

`skills-secrets-config`
