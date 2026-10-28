// Small pure helpers (no D-Bus, no GUI) so they can be unit tested.
#pragma once

#include <QString>
#include <QtGlobal>

namespace helpers {

struct PathParts {
    QString dir;  // parent folder relative to the mountpoint, "" for the top level
    QString name; // file name
};

// Splits a path relative to the mountpoint into folder and file name.
PathParts splitPath(const QString &path);

// "just now", "5 min ago", "3 hours ago", "yesterday", "4 days ago", or a date.
QString relativeTime(qint64 unixTime, qint64 now);

// "Checked just now", "Checked 3 min ago", ...; empty if there was no sync yet
// (lastSync <= 0).
QString checkedText(qint64 lastSync, qint64 now);

// Decimal units (kB, MB, GB) like Dolphin and OneDrive show them.
QString sizeText(quint64 bytes);

// "1.2 MB of 4.5 MB", or just "1.2 MB" if the total is unknown.
QString progressText(quint64 done, quint64 total);

// "Used 191.6 GB of 1.0 TB", empty if the quota is unknown.
QString quotaText(quint64 used, quint64 total);

// Name shown in the account selector, e.g. "me@example.com (OneDrive)".
QString instanceLabel(const QString &account, const QString &mountPoint);

} // namespace helpers
