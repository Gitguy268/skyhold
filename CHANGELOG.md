# Changelog

## 0.1.0 (unreleased)

First version.

- Encrypted local vault (age); the vault key lives only in the OS keychain and release destroys it.
- Upload to any number of rclone remotes, with names and contents encrypted by a separate cloud key (rclone crypt), or plain.
- Verify by downloading each copy and comparing SHA-256 (size and MD5 for plain uploads when the cloud has MD5). A copy found missing or damaged is uploaded again on the next push.
- Release: lists the files, asks for the password (argon2id, increasing delay after wrong tries), verifies every copy again, hashes each original again, then deletes the vault and the originals and writes a receipt. Warns when full-disk encryption is off and when snapshots may hold copies.
- Restore to the original paths or a folder; recover a whole backup on a new machine with only the remote and the cloud key.
- CLI, terminal UI and a localhost web UI with an access token.
- macOS, Linux and Windows. Every rclone backend except SMB.
