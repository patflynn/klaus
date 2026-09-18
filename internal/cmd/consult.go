package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
	"github.com/patflynn/klaus/internal/consult"
	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/project"
	"github.com/patflynn/klaus/internal/run"
	"github.com/spf13/cobra"
)

func newConsultCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "consult [flags] (\"<question>\" | --prompt-file PATH)",
		Short: "Ask another model a read-only question without launching a worker",
		Long: `Consult a model as a thinking partner, critic, or code reviewer. Answers stream
as plain text. Defaults to the first installed backend other than KLAUS_BACKEND,
using consult.order and per-backend model/effort defaults.

Use --thread NAME for a persistent conversation in the current Klaus session
(or the most recent session outside a pane). Its backend, model, effort, role,
and directory are retained; conflicting overrides are rejected. --list shows
these threads. One-shot questions also work without a Klaus session.

--file inlines attachments (200KB total); attachment and prompt-file paths are
relative to the invoking directory. --dir selects the partner's workspace;
--repo selects a registered local project. --panel queries all installed model
families other than the caller concurrently and groups their answers by backend.
Panel answers are buffered to keep each response together.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runConsult,
	}
	f := c.Flags()
	f.String("backend", "", "Model family: claude, codex, or agy (default: another installed family)")
	f.String("model", "", "Model identifier (default: backends.<kind>.model)")
	f.String("effort", "", "Reasoning effort (default: backends.<kind>.effort)")
	f.String("role", "", "partner, critic, reviewer, or a verbatim system prompt (default: consult.default_role)")
	f.String("thread", "", "Continue a named conversation in the current or most recent Klaus session")
	f.String("dir", "", "Read-only workspace (default: cwd, or saved thread directory)")
	f.String("repo", "", "Registered local project name to consult")
	f.StringArray("file", nil, "Inline a file in the prompt; repeatable, 200KB total")
	f.String("prompt-file", "", "Read the question from a file instead of a positional argument")
	f.Bool("panel", false, "Ask all installed families other than the caller concurrently")
	f.Bool("list", false, "List stored threads with backend, turn count, and last-used time")
	c.MarkFlagsMutuallyExclusive("dir", "repo")
	c.MarkFlagsMutuallyExclusive("panel", "thread")
	c.MarkFlagsMutuallyExclusive("panel", "backend")
	return c
}

func runConsult(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd.SetContext(ctx)
	str := func(name string) string { v, _ := cmd.Flags().GetString(name); return v }
	panel, _ := cmd.Flags().GetBool("panel")
	list, _ := cmd.Flags().GetBool("list")
	name := str("thread")
	var store consult.Store
	baseDir := ""
	if s, err := sessionStore(); err == nil {
		baseDir = s.(*run.HomeDirStore).BaseDir()
		store = consult.NewStore(baseDir)
	} else if list || name != "" {
		return err
	}
	if list {
		if len(args) > 0 {
			return fmt.Errorf("--list cannot be combined with a question")
		}
		for _, flag := range []string{"backend", "model", "effort", "role", "thread", "dir", "repo", "file", "prompt-file", "panel"} {
			if cmd.Flags().Changed(flag) {
				return fmt.Errorf("--list cannot be combined with --%s", flag)
			}
		}
		threads, err := store.List()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(threads))
		for n := range threads {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintln(cmd.OutOrStdout(), "THREAD\tBACKEND\tTURNS\tLAST-USED")
		for _, n := range names {
			t := threads[n]
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d\t%s\n", n, t.Backend, t.Turns, t.LastUsed.Format(time.RFC3339))
		}
		return nil
	}
	prompt, err := resolvePrompt(args, str("prompt-file"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("question must not be empty")
	}
	files, _ := cmd.Flags().GetStringArray("file")
	prompt, err = consult.InlineFiles(prompt, files)
	if err != nil {
		return err
	}
	var saved *consult.Thread
	if name != "" {
		unlock, err := store.Lock(name)
		if err != nil {
			return err
		}
		defer unlock()
		saved, err = store.Load(name)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	dir := str("dir")
	if str("repo") != "" {
		reg, err := project.Load()
		if err != nil {
			return err
		}
		_, dir = resolveRepoTarget(str("repo"), "", reg)
		if dir == "" {
			return fmt.Errorf("project %q is not registered; use klaus project add or --dir", str("repo"))
		}
	}
	if dir == "" && saved != nil {
		dir = saved.Dir
	}
	if dir == "" {
		dir = "."
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace %s is not a directory", dir)
	}
	cfgDir := dir
	if root, err := exec.CommandContext(cmd.Context(), "git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		cfgDir = strings.TrimSpace(string(root))
	}
	cfg, err := config.Load(cfgDir)
	if err != nil {
		return err
	}
	role := str("role")
	if role == "" {
		role = cfg.Consult.DefaultRole
	}
	if role == "" {
		role = "partner"
	}
	var kinds []backend.Kind
	switch {
	case saved != nil:
		if saved.BackendSessionID == "" {
			return fmt.Errorf("thread %q has no backend session ID", name)
		}
		if kind, err := backend.Parse(string(saved.Backend)); err != nil || kind != saved.Backend {
			return fmt.Errorf("invalid backend in saved thread %q", name)
		}
		for flag, value := range map[string]string{"backend": string(saved.Backend), "model": saved.Model, "effort": saved.Effort, "role": saved.Role} {
			if cmd.Flags().Changed(flag) && str(flag) != value {
				return fmt.Errorf("--%s conflicts with saved thread %q", flag, name)
			}
		}
		if dir != saved.Dir {
			return fmt.Errorf("workspace conflicts with saved thread %q", name)
		}
		kinds = []backend.Kind{saved.Backend}
	case str("backend") != "":
		kind, err := backend.Parse(str("backend"))
		if err != nil {
			return err
		}
		kinds = []backend.Kind{kind}
	default:
		order := cfg.Consult.Order
		if len(order) == 0 {
			order = []string{"codex", "claude", "agy"}
		}
		if panel {
			order = append(append([]string{}, order...), "codex", "claude", "agy")
		}
		kinds, err = consult.Installed(order, os.Getenv("KLAUS_BACKEND"))
		if err != nil {
			return err
		}
		if len(kinds) == 0 {
			return fmt.Errorf("no other backend installed; install one or choose --backend explicitly")
		}
		if !panel {
			kinds = kinds[:1]
		}
	}
	threads := make([]*consult.Thread, 0, len(kinds))
	for _, kind := range kinds {
		defaults := cfg.AgentDefaults(string(kind))
		t := &consult.Thread{Backend: kind, Model: defaults.Model, Effort: defaults.Effort, Role: role, Dir: dir, CreatedAt: time.Now().UTC()}
		if cmd.Flags().Changed("model") {
			t.Model = str("model")
		}
		if cmd.Flags().Changed("effort") {
			t.Effort = str("effort")
		}
		if saved != nil {
			t = saved
		}
		if err := kind.ValidateEffort(t.Effort); err != nil {
			return err
		}
		threads = append(threads, t)
	}
	ask := func(t *consult.Thread, out, errOut io.Writer) error {
		start := time.Now()
		id := ""
		if name != "" && t.Backend == backend.Claude && t.BackendSessionID == "" {
			id = genUUIDv4()
		}
		answer, id, askErr := consult.Ask(cmd.Context(), t, prompt, name != "", id, out, errOut)
		if baseDir != "" {
			emitEvent(baseDir, "", event.ConsultCompleted, map[string]interface{}{"backend": t.Backend, "role": t.Role, "thread": name, "duration_ms": time.Since(start).Milliseconds(), "success": askErr == nil})
		}
		if name != "" {
			if err := store.Append(name, prompt, answer); err != nil {
				return errors.Join(askErr, err)
			}
			if askErr == nil {
				t.BackendSessionID = id
				t.Turns++
				t.LastUsed = time.Now().UTC()
				if err := store.Save(name, t); err != nil {
					return err
				}
			}
		}
		return askErr
	}
	if !panel {
		return ask(threads[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
	}
	type result struct {
		thread         *consult.Thread
		answer, stderr string
		err            error
	}
	results := make(chan result, len(threads))
	for _, t := range threads {
		go func() {
			var out, errOut bytes.Buffer
			err := ask(t, &out, &errOut)
			results <- result{t, out.String(), errOut.String(), err}
		}()
	}
	var errs []error
	for range threads {
		r := <-results
		model := r.thread.Model
		if model == "" {
			model = "default"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "## %s (%s)\n\n%s\n", r.thread.Backend, model, r.answer)
		if r.stderr != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "[%s]\n%s", r.thread.Backend, r.stderr)
		}
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	return errors.Join(errs...)
}

func init() { rootCmd.AddCommand(newConsultCmd()) }
