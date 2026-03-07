# Klaus Project Guidelines

## Commits & PRs
- Do NOT include `Co-Authored-By` lines mentioning Claude or Anthropic in commits
- Do NOT mention AI in commit messages or PR descriptions

## Testing
- Prefer integration and e2e tests that exercise real behavior over unit tests with mocked internals
- Only unit test genuinely tricky logic — don't write tests that just mirror the implementation
- A few tests that run the real binary are worth more than many tests with injected fakes
- Run `go test ./...` before committing
- PRs without tests for new code will not be merged

## Documentation
- If you add or change a CLI command or flag, update the help text in the cobra command definition
- If you add or change user-facing behavior, update the README if one exists
- Keep code comments accurate — update or remove comments that no longer match the code
