package cmd

import (
	"log/slog"

	"github.com/patflynn/klaus/internal/event"
	"github.com/patflynn/klaus/internal/integration"
	"github.com/patflynn/klaus/internal/pipeline"
	"github.com/patflynn/klaus/internal/run"
	"github.com/patflynn/klaus/internal/valley"
)

// reportChangeTransitions emits the change:* events for the valley changes
// of the runs and tracked changes an integration check followed: for each
// change the valley's listing holds in a repo it could read, the
// transitions its record shows that the ledger didn't hold
// (pipeline.ChangeLedger.Observe). It reads the change's own record, not a
// run's version, so a change restacked under its run reports its landing
// once, as the change's.
//
// A version is this session's own when one of its runs pushed its head or
// records it (run.State.Valley), as finalize and klaus ready do. The runs
// are read after the listing, and finalize saves a run's pushed head before
// it marks the head ready, so no version of the session's is taken for
// another's. If they can't be read, nothing is reported until the next
// check.
func reportChangeTransitions(store *run.HomeDirStore, ledger *pipeline.ChangeLedger, followed []*run.State, listing *valley.Changes) {
	keys := followedChanges(followed)
	if len(keys) == 0 {
		return
	}
	mine, err := store.List()
	if err != nil {
		slog.Warn("reading the session's runs to report change transitions", "err", err)
		return
	}
	listed := integration.ListedChanges(listing)
	log := event.NewLog(store.BaseDir())
	for _, k := range keys {
		ch, ok := listed[k]
		if !ok {
			continue
		}
		ts, err := ledger.Observe(k.Repo, ch, func(v valley.Version) bool { return recordsVersion(mine, k, v) })
		if err != nil {
			slog.Warn("recording a change's transitions", "repo", k.Repo, "change_id", k.ChangeID, "err", err)
		}
		id := ch.Handle
		if id == "" {
			id = ch.ChangeID
		}
		for _, t := range ts {
			if err := log.Emit(event.New(id, t.Type, changeTransitionData(k.Repo, ch, t))); err != nil {
				slog.Warn("emitting a change event", "type", t.Type, "change_id", k.ChangeID, "err", err)
			}
		}
	}
}

// followedChanges names the changes of states, once each, in order.
func followedChanges(states []*run.State) []integration.ChangeKey {
	seen := make(map[integration.ChangeKey]bool)
	var keys []integration.ChangeKey
	for _, s := range states {
		k := integration.ChangeKey{Repo: integration.RepoName(s), ChangeID: s.ChangeID}
		if k.Repo == "" || k.ChangeID == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	return keys
}

// recordsVersion reports whether a run among states pushed version v of
// change k, or records it as the version its pushed head became. A run
// whose recorded valley repo is another pushed another repo's change; one
// finalize hasn't recorded a version for yet counts by its pushed head. A
// tracked change's record names the version klaus track found, which it
// didn't record.
func recordsVersion(states []*run.State, k integration.ChangeKey, v valley.Version) bool {
	for _, s := range states {
		if s.ChangeID != k.ChangeID || s.Type == "track" || s.Valley != nil && s.Valley.Repo != "" && s.Valley.Repo != k.Repo {
			continue
		}
		if s.PushedSHA != nil && *s.PushedSHA == v.Head ||
			s.Valley != nil && s.Valley.Repo == k.Repo && s.Valley.Version == v.N {
			return true
		}
	}
	return false
}

// changeTransitionData is what a change:* event carries: the change, and
// the version the transition concerns, with repo_key and key as run events
// name a valley change.
func changeTransitionData(repo string, ch valley.Change, t pipeline.ChangeTransition) map[string]interface{} {
	d := map[string]interface{}{
		"repo":      repo,
		"handle":    ch.Handle,
		"change_id": ch.ChangeID,
		"version":   t.Version.N,
		"head":      t.Version.Head,
		"title":     t.Version.Title,
		"repo_key":  "valley:" + repo,
		"key":       changeKey("valley:"+repo, "", ch.ChangeID),
	}
	switch t.Type {
	case event.ChangeIntegrated:
		d["landed_commit"] = t.Commit
	case event.ChangeNewVersion:
		d["uploader"] = t.Version.Uploader
	}
	return d
}
