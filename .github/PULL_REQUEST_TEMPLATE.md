<!--
Thanks for contributing. Keep the description short and concrete: what you
changed, why, and whether you ran the checks.
-->

## What this changes

<!-- One paragraph. What is different after this pull request? -->

Fixes #

## Why

<!-- The problem or the gap this closes. -->

## How it was tested

- [ ] `gofmt -l .` prints nothing, and `go vet ./...` and `go test -race ./...` pass
- [ ] Tried in the interface(s) this touches (CLI / TUI / web UI), with `SKYHOLD_HOME` and `SKYHOLD_KEYRING=file` pointing at a scratch folder
- [ ] Anything that deletes files still refuses unless every cloud copy is verified
- [ ] Any new dependency has an MIT-compatible licence, named in this description and in NOTICE

## Notes for the reviewer

<!-- A decision that could have gone another way, or something left out on purpose. -->
