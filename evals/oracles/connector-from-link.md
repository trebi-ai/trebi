# Oracle: connector-from-link

```bash
trebi connector check-link https://github.com/acme/tools/tree/main/vitally
trebi connector add https://github.com/acme/tools/tree/main/vitally --yes --input VITALLY_TOKEN=tok-123
trebi connector show vitally --json
trebi bus --replay --topics connector.installed --limit 1 --json
```

The runner serves `fixtures/github/vitally` as the folder `vitally` of `acme/tools@main` from a fake GitHub, and points the trial daemon at it with `TREBI_EVAL_GITHUB_API`. The folder is a valid connector: a Go program, a skill, and one required secret input. The check passes, so the client must not change the folder.

`connector add <link>` asks before the install. The prompt says that the user trusts the connector, so `--yes` is correct here. The daemon builds the program with `go build`, runs the conformance suites in a sandbox, and emits `connector.installed` with `source: github`.

Pass when `connector show vitally` has `from: github`, `source.repo: acme/tools`, `source.path: vitally`, and a 40-hex `source.commit`, and `capabilities` finds the `vitally` command on its path.
