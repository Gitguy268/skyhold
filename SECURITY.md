# Security policy

## Reporting a vulnerability

Please report security problems privately, not in a public issue.

Use GitHub's private reporting form: <https://github.com/Gitguy268/skyhold/security/advisories/new>

Include what you found, how to reproduce it, your OS and the rclone backend involved. Leave out your own keys, passwords and remote strings. You should get a reply within a week. Please give the fix time to ship before you publish details.

## Scope

The most useful reports are about:

- `release` deleting an original that is not verified in the cloud, deleting after a wrong password, or deleting a file outside the staged list;
- vault contents, the vault key, the cloud key or remote credentials leaking to disk, logs, the cloud or the web UI;
- the web UI (`skyhold serve`): reaching the API without the token, from another website (DNS rebinding, CSRF), or from another user on the same machine;
- the cloud key or rclone crypt setup being weaker than described in the README;
- the GitHub Actions workflows in this repository.

The README's [Limits](README.md#limits) section lists what Skyhold does not protect against: recovery of deleted originals from an unencrypted SSD, copies in snapshots and backups, and the file keyring (`SKYHOLD_KEYRING=file`). Problems in rclone, age or a cloud provider themselves belong with those projects.

## Supported versions

Only the latest version on the `main` branch is supported.
