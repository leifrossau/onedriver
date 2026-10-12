// SPDX-License-Identifier: GPL-3.0-or-later
// Pure helpers shared by the Dolphin overlay and file item action plugins.
#pragma once

#include <QString>
#include <QStringList>

namespace OneDriverDolphin {

inline constexpr const char *kBusPrefix = "io.github.onedriver.Mount.";
inline constexpr const char *kObjectPath = "/io/github/onedriver/Mount";
inline constexpr const char *kInterface = "io.github.onedriver.Mount1";

inline constexpr const char *kEmblemCloud = "onedriver-emblem-cloud";
inline constexpr const char *kEmblemPinned = "emblem-success";
inline constexpr const char *kEmblemSync = "emblem-synchronizing-symbolic";
inline constexpr const char *kEmblemError = "emblem-error";

struct FileState {
    QString state; // cloud, local, modified, uploading, error; empty if unknown
    bool pinned = false;
    bool valid() const { return !state.isEmpty(); }
};

/// Overlay icon names for a state. Directories only show sync/error/pinned:
/// "cloud" is meaningless for a folder.
QStringList overlaysFor(const FileState &st, bool isDir);

/// Longest mountpoint containing path (boundary aware: /a/b does not match
/// the mountpoint /a/bc). Returns "" if none. Mountpoints have no trailing /.
QString mountForPath(const QString &path, const QStringList &mounts);

/// Absolute path for a path relative to a mountpoint ("" or "." is the mount).
QString absolutePath(const QString &mountPoint, const QString &relative);

/// Path relative to the mountpoint ("" for the mountpoint itself).
QString relativePath(const QString &mountPoint, const QString &path);

/// True if the bus name is an onedriver instance (io.github.onedriver.Mount.*).
bool isMountBusName(const QString &name);

/// Reads the state xattrs without following symlinks. Invalid if absent.
FileState readFileState(const QString &path);

bool setPinnedXattr(const QString &path, bool pinned);
bool setEvictXattr(const QString &path);

} // namespace OneDriverDolphin
