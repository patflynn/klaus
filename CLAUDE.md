# Klaus Project Guidelines

## Commits & PRs
- Do NOT include `Co-Authored-By` lines mentioning Claude or Anthropic in commits
- Do NOT mention AI in commit messages or PR descriptions

## Testing
- All new functions and commands must have corresponding tests in *_test.go files
- Run `go test ./...` before committing
- Tests should cover happy path and error cases
- PRs without tests for new code will not be merged
