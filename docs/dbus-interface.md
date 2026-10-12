# onedriver D-Bus interface (v1)

Every running `onedriver` instance (one per mountpoint) exports a status and
control interface on the **session bus**. It is used by `onedriver-tray` and
later by the Dolphin overlay plugin.

## Discovery

- Bus name: `io.github.onedriver.Mount.<key>`, where `<key>` is the mountpoint
  escaped with `systemd-escape --path` rules and then mapped to D-Bus name
  rules: every character not in `[A-Za-z0-9_]` becomes `_`, and a leading
  digit gets a `_` prefix. Example: `/home/leif/OneDrive` →
  `io.github.onedriver.Mount.home_leif_OneDrive`.
- Clients find instances with `ListNames` and the prefix
  `io.github.onedriver.Mount.`, and watch `NameOwnerChanged` for instances
  appearing and disappearing.
- Object path: `/io/github/onedriver/Mount`
- Interface: `io.github.onedriver.Mount1`
- The service also implements `org.freedesktop.DBus.Properties` (with
  `PropertiesChanged`) and `org.freedesktop.DBus.Introspectable`.

If the session bus is unavailable, onedriver logs a warning and keeps running
without the interface.

## Properties (read-only)

| Name | Type | Meaning |
|---|---|---|
| `MountPoint` | `s` | absolute mountpoint |
| `Account` | `s` | account e-mail / UPN, `""` if unknown |
| `State` | `s` | overall state, see below |
| `Paused` | `b` | sync is paused by the user |
| `PendingUploads` | `u` | upload sessions queued, in progress or retrying |
| `PendingDownloads` | `u` | pinned files waiting to be hydrated |
| `PinnedFiles` | `u` | effectively pinned files |
| `PinnedBytes` | `t` | size of effectively pinned files |
| `CacheBytes` | `t` | size of the content cache on disk |
| `QuotaUsed` | `t` | OneDrive quota used (bytes), 0 if unknown |
| `QuotaTotal` | `t` | OneDrive quota total (bytes), 0 if unknown |
| `LastSync` | `x` | unix time of the last successful delta poll, 0 = never |
| `LastError` | `s` | last error message, `""` if none |

`State` is one of:

- `idle`: online, nothing pending
- `syncing`: uploads or pinned downloads pending
- `paused`: the user paused syncing
- `offline`: no connection to OneDrive
- `error`: an upload was dropped after too many retries, or authentication
  failed; `LastError` says why

`PropertiesChanged` is emitted when any of these change, rate-limited to at
most once per second (coalesced).

## Methods

| Method | Signature | Meaning |
|---|---|---|
| `GetStatus` | `() → a{sv}` | all properties above in one call |
| `Pause` | `() → ()` | stop starting new uploads, delta polling and hydration. Writes still go to the local cache and are uploaded after `Resume`. |
| `Resume` | `() → ()` | undo `Pause`, trigger a delta poll immediately |
| `SyncNow` | `() → ()` | trigger a delta poll immediately |
| `GetRecent` | `(u limit) → a(sxs)` | recently synced items, newest first: (path relative to the mountpoint, unix time, action) where action is `uploaded`, `downloaded`, `deleted`, `conflict` or `error` |
| `GetFileState` | `(s path) → (s state, b pinned)` | state as in the `user.onedriver.state` xattr, path is absolute or relative to the mountpoint |
| `SetPinned` | `(s path, b pinned) → ()` | same as setting `user.onedriver.pinned` |
| `Evict` | `(s path) → ()` | same as setting `user.onedriver.evict` |
| `GetWebURL` | `(s path) → s` | the item's OneDrive web URL (`webUrl`), for "View online" |

Errors use `org.freedesktop.DBus.Error.FileNotFound` for unknown paths and
`org.freedesktop.DBus.Error.Failed` otherwise.

### Transfers (v1.1)

| Method | Signature | Meaning |
|---|---|---|
| `GetTransfers` | `() → a(sssttx)` | uploads and downloads that are queued, running or waiting to retry: (path relative to the mountpoint, direction `upload` or `download`, state `queued`, `active` or `retrying`, bytes done, bytes total, unix time the transfer was queued). Active transfers first, then by queue time. |

Downloads include files opened by applications and pinned files being
hydrated. Bytes done is updated while data is transferred (per chunk for
large uploads, continuously for downloads); small uploads go from 0 to done
in one step. Finished transfers disappear from the list and show up in
`GetRecent`.

## Signals

| Signal | Signature | Meaning |
|---|---|---|
| `Notify` | `(s kind, s path, s message)` | something the user should know about. `kind` is `conflict` (a conflict copy was created, `path` is the copy), `upload-failed` (an upload was dropped), `auth-expired` (login needed again) or `error`. |

Clients show `Notify` as desktop notifications.

| Signal | Signature | Meaning |
|---|---|---|
| `TransfersChanged` | `()` | the result of `GetTransfers` changed (a transfer was added, finished or made progress). Coalesced to at most 4 per second. Clients call `GetTransfers` and `GetRecent` again. |

## Activity window

`onedriver-activity` (the popup shown when the tray icon is clicked) owns
`io.github.onedriver.Activity` on the session bus, object path
`/io/github/onedriver/Activity`, interface `io.github.onedriver.Activity1`
with one method, `Toggle() → ()`, which shows the window if hidden and hides
it if shown. A second `onedriver-activity` process calls `Toggle` on the
running one and exits. The tray's left click (SNI `Activate`) calls `Toggle`,
and starts `onedriver-activity` if the name has no owner.
