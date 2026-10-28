# onedriver D-Bus interface (v1.5)

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

Additional property (v1.2): `AuthExpired` (`b`), true while the daemon has no
valid login. The daemon keeps running (cached files stay readable, changes are
kept locally) and resumes syncing after `Login` succeeds.

Additional property (v1.5): `Checking` (`b`), true from the moment `SyncNow` or
`Resync` is called until the poll cycle it triggered has finished, whether it
succeeded or failed. Regular background polls never set it. It stays true for
at least about a second so that a check that finds nothing is still visible,
and `PropertiesChanged` is emitted right away when it flips. It is cleared
early by `Pause`, and when the daemon cannot poll (login expired); after
`SyncNow` it also times out after 2 minutes, after `Resync` it stays true for
the whole enumeration. While `Checking` is true and nothing with higher
precedence applies (`error`, `paused`, `offline`), `State` is `syncing`.
`LastSync` is updated when the triggered poll succeeded. Clients must cope
with the property missing on older daemons.

`PropertiesChanged` is emitted when any of these change, rate-limited to at
most once per second (coalesced).

## Methods

| Method | Signature | Meaning |
|---|---|---|
| `GetStatus` | `() → a{sv}` | all properties above in one call |
| `Pause` | `() → ()` | stop starting new uploads, delta polling and hydration. Writes still go to the local cache and are uploaded after `Resume`. |
| `Resume` | `() → ()` | undo `Pause`, trigger a delta poll immediately |
| `SyncNow` | `() → ()` | trigger a delta poll immediately |
| `Resync` | `() → ()` | (v1.4) forget the delta position and enumerate the whole drive again, then poll right away. A full enumeration reports every item's current state, so items that a past poll missed are created in folders that are loaded. Items in loaded folders that the enumeration did not report are deleted locally if the server confirms (404) that they are gone; items with local changes, pending uploads or open files are never removed. If the enumeration fails halfway it is restarted from the beginning on the next poll. Clients should treat `org.freedesktop.DBus.Error.UnknownMethod` as "daemon too old". |
| `GetRecent` | `(u limit) → a(sxs)` | recently synced items, newest first: (path relative to the mountpoint, unix time, action) where action is `uploaded`, `downloaded`, `deleted`, `conflict` or `error` |
| `GetFileState` | `(s path) → (s state, b pinned)` | state as in the `user.onedriver.state` xattr, path is absolute or relative to the mountpoint |
| `SetPinned` | `(s path, b pinned) → ()` | same as setting `user.onedriver.pinned` |
| `Evict` | `(s path) → ()` | same as setting `user.onedriver.evict` |
| `GetWebURL` | `(s path) → s` | the item's OneDrive web URL (`webUrl`), for "View online" |
| `Login` | `() → ()` | (v1.2) open the login window now, from the daemon process. Used after an `auth-expired` notification. Returns immediately; the result shows up as `State` leaving `error` and `LastError` being cleared, or as another `auth-expired` notification. Calling it while a login window is already open does nothing. Fails with `org.freedesktop.DBus.Error.Failed` right away if no window can be opened (no `DISPLAY`/`WAYLAND_DISPLAY`, or the daemon runs with `-n`); the message says to run `onedriver -n <mountpoint>` in a terminal. |

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
`GetRecent`. Files that applications read only partly are fetched in ranges:
while ranges of such a file are being fetched, it is listed as an active
download with bytes total = the file size and bytes done = the bytes of it
that are cached so far (it shows up in `GetRecent` as downloaded once the
whole file has been fetched).

## Signals

| Signal | Signature | Meaning |
|---|---|---|
| `Notify` | `(s kind, s path, s message)` | something the user should know about. `kind` is `conflict` (a conflict copy was created, `path` is the copy), `upload-failed` (an upload was dropped), `auth-expired` (login needed again) or `error`. |

Clients show `Notify` as desktop notifications.

| Signal | Signature | Meaning |
|---|---|---|
| `TransfersChanged` | `()` | the result of `GetTransfers` changed (a transfer was added, finished or made progress). Coalesced to at most 4 per second. Clients call `GetTransfers` and `GetRecent` again. |

### File states (v1.3)

| Signal | Signature | Meaning |
|---|---|---|
| `FileStatesChanged` | `(as paths)` | the `user.onedriver.state` or `user.onedriver.pinned` of these items changed (paths relative to the mountpoint). Emitted for: download finished (cloud → local), eviction (local → cloud), local write (→ modified), upload queued/started/finished/failed, pin/unpin (for a folder: the folder itself plus the children that are loaded in memory), conflict copy created. Coalesced: at most 4 signals per second, each with up to 500 paths; when more items changed in that interval the signal carries the parent folders instead. |

File managers read the per-file state with the `user.onedriver.state` and
`user.onedriver.pinned` extended attributes (cheap, no D-Bus round trip per
file) and listen to `FileStatesChanged` to refresh what is on screen.

## Activity window

`onedriver-activity` (the popup shown when the tray icon is clicked) owns
`io.github.onedriver.Activity` on the session bus, object path
`/io/github/onedriver/Activity`, interface `io.github.onedriver.Activity1`
with two methods: `Toggle() → ()` shows the window in the bottom right corner
of the primary screen if hidden and hides it if shown; `ToggleAt(i x, i y) → ()`
does the same but opens the window next to the point (x, y), in global screen
coordinates, on the screen that contains it. A second `onedriver-activity` process calls `Toggle` on the
running one and exits. The tray's left click (SNI `Activate`) calls `ToggleAt` with the position of
the click (`Toggle` if the host reports none), and starts `onedriver-activity
--show --at=X,Y` if the name has no owner.
