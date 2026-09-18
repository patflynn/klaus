package review

import (
	"fmt"
	"os/exec"

	"github.com/patflynn/klaus/internal/backend"
	"github.com/patflynn/klaus/internal/config"
)

// ChooseReviewer picks the reviewer family and model for code written by author.
// Family: backends.<author>.review_backend > first cross_review.order entry ≠ author with its CLI on PATH > author.
func ChooseReviewer(author backend.Kind, cfg config.Config) (backend.Kind, string, error) {
	kind, err := reviewerKind(author, cfg)
	if err != nil {
		return "", "", err
	}
	return kind, ReviewModel(kind, cfg), nil
}

func reviewerKind(author backend.Kind, cfg config.Config) (backend.Kind, error) {
	if name := cfg.BackendDefaults[string(author)].ReviewBackend; name != "" {
		k, err := backend.Parse(name)
		if err != nil {
			return "", fmt.Errorf("backends.%s.review_backend: %w", author, err)
		}
		return k, nil
	}
	if cfg.CrossReviewEnabled() {
		for _, name := range cfg.CrossReviewOrder() {
			k, err := backend.Parse(name)
			if err != nil {
				return "", fmt.Errorf("cross_review.order: %w", err)
			}
			if k == author {
				continue
			}
			if _, err := exec.LookPath(string(k)); err == nil { // binary name == kind
				return k, nil
			}
		}
	}
	return author, nil
}

// ReviewModel: backends.<kind>.review_model > pre_review.review_model (claude only; default haiku) > "" (CLI default).
func ReviewModel(kind backend.Kind, cfg config.Config) string {
	if m := cfg.BackendDefaults[string(kind)].ReviewModel; m != "" {
		return m
	}
	if kind == backend.Claude {
		return cfg.PreReviewModel()
	}
	return ""
}
