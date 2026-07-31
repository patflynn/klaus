#!/usr/bin/env bash
# Reproduces the finding in docs/AGENT_MESSAGING.md: tmux pane injection reaches
# an interactive claude session but not a headless `claude -p` agent, which is
# how klaus launches every agent.
#
# Usage: ./scripts/repro-agent-message-injection.sh
#
# Not a CI test — it spends real API credit (~$0.30) and needs a logged-in
# `claude` plus `tmux`. Runs in a throwaway dir on an isolated tmux server.

set -euo pipefail

command -v claude >/dev/null || { echo "claude not found on PATH" >&2; exit 1; }
command -v tmux >/dev/null || { echo "tmux not found on PATH" >&2; exit 1; }

DIR=$(mktemp -d)
SOCK="$DIR/sock"
MARKER="INJECTED-MARKER-$$"
trap 'tmux -S "$SOCK" -f /dev/null kill-server 2>/dev/null || true; rm -rf "$DIR"' EXIT

# A two-line message. If injection splits it at the newline the agent sees two
# turns instead of one, which is the other thing worth catching here.
printf '%s line one\nline two of the same message\n' "$MARKER" > "$DIR/msg.txt"

# Busy-work prompt: the agent blocks on a gate file so we can inject mid-run,
# then reports back whatever extra user message it received.
PROMPT="Use the Bash tool to run this exact command: until [ -f $DIR/go ]; do sleep 2; done; echo ready. After it finishes, reply with EXACTLY the text of any additional user message you received while you were working, or the single word NONE."

tmux -S "$SOCK" -f /dev/null new-session -d -s repro -x 200 -y 50

# Injects msg.txt into a pane as a single bracketed paste, then submits it with
# a separate Enter. -p asks tmux for bracketed paste so a TUI reads it as one
# paste rather than a run of Enter presses; the text goes through a buffer so a
# leading dash is never parsed as a flag.
inject() {
  tmux -S "$SOCK" -f /dev/null load-buffer -b repro "$DIR/msg.txt"
  tmux -S "$SOCK" -f /dev/null paste-buffer -d -p -b repro -t "$1"
  sleep 2
  tmux -S "$SOCK" -f /dev/null send-keys -t "$1" Enter
}

echo "=== Experiment 1: headless 'claude -p' (how klaus launches agents) ==="
cat > "$DIR/headless.sh" <<EOF
#!/usr/bin/env bash
cd "$DIR"
claude -p --dangerously-skip-permissions --verbose --output-format stream-json \
  --max-budget-usd 1.00 "\$1" > "$DIR/out.jsonl" 2>"$DIR/err.txt"
echo "EXIT=\$?" >> "$DIR/err.txt"
EOF
chmod +x "$DIR/headless.sh"

PANE=$(tmux -S "$SOCK" -f /dev/null split-window -t repro -v -d -P -F '#{pane_id}' \
  -c "$DIR" "$DIR/headless.sh '$PROMPT'")
sleep 15
inject "$PANE"
sleep 5
touch "$DIR/go"
until grep -q '^EXIT=' "$DIR/err.txt" 2>/dev/null; do sleep 2; done

if grep -q "$MARKER" "$DIR/out.jsonl"; then
  echo "RESULT: marker FOUND in headless trajectory — the finding no longer holds."
else
  echo "RESULT: marker ABSENT from headless trajectory — pane injection is inert."
fi
rm -f "$DIR/go"

echo
echo "=== Experiment 2: interactive session (how the coordinator runs) ==="
cat > "$DIR/interactive.sh" <<EOF
#!/usr/bin/env bash
cd "$DIR"
exec claude --dangerously-skip-permissions "\$1"
EOF
chmod +x "$DIR/interactive.sh"

PANE=$(tmux -S "$SOCK" -f /dev/null split-window -t repro -v -d -P -F '#{pane_id}' \
  -c "$DIR" "$DIR/interactive.sh '$PROMPT'")
# The trust-folder dialog can swallow the first paste, so wait for the agent to
# actually be working before injecting.
until tmux -S "$SOCK" -f /dev/null capture-pane -t "$PANE" -p | grep -q 'esc to interrupt'; do
  sleep 2
done
inject "$PANE"
sleep 5
touch "$DIR/go"

# Poll rather than sleeping a fixed amount: the transcript is written as the
# turn progresses, so a fixed wait risks reporting ABSENT before it lands.
PROJECT_DIR="$HOME/.claude/projects/$(echo "$DIR" | tr '/.' '--')"
TRANSCRIPT=""
for _ in $(seq 60); do
  TRANSCRIPT=$(ls -t "$PROJECT_DIR"/*.jsonl 2>/dev/null | head -1 || true)
  [ -n "$TRANSCRIPT" ] && grep -q "$MARKER" "$TRANSCRIPT" && break
  sleep 2
done

if [ -n "$TRANSCRIPT" ] && grep -q "$MARKER" "$TRANSCRIPT"; then
  echo "RESULT: marker FOUND in interactive transcript (as a queued_command attachment):"
  grep -o "\"type\":\"queued_command\".\{0,160\}" "$TRANSCRIPT" | head -1
else
  echo "RESULT: marker ABSENT from interactive transcript (transcript: ${TRANSCRIPT:-none found})."
fi

echo
echo "Final pane state:"
tmux -S "$SOCK" -f /dev/null capture-pane -t "$PANE" -p | tail -12
