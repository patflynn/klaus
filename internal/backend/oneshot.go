package backend

type OneShotOptions struct {
	Prompt, SystemPrompt, Model, Effort, ResumeID, SessionID, LastMessage, AgyAgent, DiagnosticLog string
	Threaded                                                                                       bool
}

// OneShot emits text; Claude/Codex read stdin, agy takes a prompt argument.
func OneShot(k Kind, o OneShotOptions) []string {
	var a []string
	switch k {
	case Claude:
		a = []string{"claude", "-p", "--safe-mode", "--output-format", "text", "--system-prompt", o.SystemPrompt,
			"--tools", "Read,Grep,Glob", "--permission-mode", "dontAsk", "--strict-mcp-config", "--disable-slash-commands"}
		if !o.Threaded {
			a = append(a, "--no-session-persistence")
		} else if o.ResumeID != "" {
			a = append(a, "--resume", o.ResumeID)
		} else if o.SessionID != "" {
			a = append(a, "--session-id", o.SessionID)
		}
	case Codex:
		a = []string{"codex", "exec", "--sandbox", "read-only", "-c", `approval_policy="never"`,
			"-c", "mcp_servers={}", "-c", "features.apps=false", "-c", "features.plugins=false", "-c", "features.hooks=false", "-c", "features.multi_agent=false",
			"-c", "developer_instructions=" + tomlString(o.SystemPrompt)}
		if o.ResumeID != "" {
			a = append(a, "resume", o.ResumeID)
		}
		a = append(a, "--skip-git-repo-check")
		if !o.Threaded {
			a = append(a, "--ephemeral")
		}
		if o.LastMessage != "" {
			a = append(a, "--output-last-message", o.LastMessage)
		}
	case Agy:
		if o.AgyAgent == "" {
			panic("agy OneShot requires PrepareAgyReadOnly")
		}
		a = []string{"agy", "--add-dir", ".", "--output-format", "text", "--mode", "plan", "--sandbox", "--agent", o.AgyAgent}
		if o.ResumeID != "" {
			a = append(a, "--conversation", o.ResumeID)
		}
		if o.DiagnosticLog != "" {
			a = append(a, "--log-file", o.DiagnosticLog)
		}
	}
	a = k.modelArgs(a, Options{Model: o.Model, Effort: o.Effort})
	if k == Agy {
		return append(a, "--print", o.SystemPrompt+"\n\n"+o.Prompt)
	}
	if k == Codex {
		return append(a, "-")
	}
	return a
}
