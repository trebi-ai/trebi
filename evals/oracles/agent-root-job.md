# Oracle: agent-root-job

The oracle does not change state: it reads the doctor report, then writes the
diagnosis the task asks for.

```bash
trebi doctor --json | jq -e '[.findings[] | select(.code == "agent_root_job")] | length >= 1'
trebi agent show cmo | jq -e '(.parse_error // "") | contains("same directory")'
trebi job list --json | jq -e '[.jobs[] | select(.name == "agents/cmo")] | length == 0'
cat > "$EVAL_WORKDIR/agent-diagnosis.txt" << 'EOF'
Cause: agents/cmo/JOB.md sits beside agents/cmo/AGENT.md. An agent folder root
holds AGENT.md only, so the stray JOB.md is not indexed and the agent row
carries a parse error. Fix: move the JOB.md into a subfolder of agents/cmo,
for example agents/cmo/weekly-digest/JOB.md. Then rebuild the index.
EOF
```
