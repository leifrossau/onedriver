// SPDX-License-Identifier: GPL-3.0-or-later
#include "logic.h"

#include <QByteArray>
#include <QFile>

#include <sys/xattr.h>

namespace OneDriverDolphin {

QStringList overlaysFor(const FileState &st, bool isDir)
{
    if (!st.valid())
        return {};
    if (st.state == QLatin1String("error"))
        return {QString::fromLatin1(kEmblemError)};
    if (st.state == QLatin1String("modified") || st.state == QLatin1String("uploading"))
        return {QString::fromLatin1(kEmblemSync)};
    if (st.state == QLatin1String("cloud") && !isDir)
        return {QString::fromLatin1(kEmblemCloud)};
    if (st.pinned && (st.state == QLatin1String("local") || isDir))
        return {QString::fromLatin1(kEmblemPinned)};
    return {};
}

QString mountForPath(const QString &path, const QStringList &mounts)
{
    QString best;
    for (const QString &m : mounts) {
        if (m.isEmpty())
            continue;
        const bool inside = path == m || (path.size() > m.size() && path.startsWith(m) && path.at(m.size()) == QLatin1Char('/'));
        if (inside && m.size() > best.size())
            best = m;
    }
    return best;
}

QString absolutePath(const QString &mountPoint, const QString &relative)
{
    if (relative.isEmpty() || relative == QLatin1String("."))
        return mountPoint;
    if (relative.startsWith(QLatin1Char('/')))
        return mountPoint + relative;
    return mountPoint + QLatin1Char('/') + relative;
}

QString relativePath(const QString &mountPoint, const QString &path)
{
    if (path == mountPoint)
        return {};
    return path.mid(mountPoint.size() + 1);
}

bool isMountBusName(const QString &name)
{
    return name.startsWith(QLatin1String(kBusPrefix)) && name.size() > int(qstrlen(kBusPrefix));
}

static QByteArray getx(const QByteArray &path, const char *name)
{
    char buf[64];
    const ssize_t n = lgetxattr(path.constData(), name, buf, sizeof(buf));
    if (n < 0)
        return {};
    return QByteArray(buf, int(n));
}

FileState readFileState(const QString &path)
{
    const QByteArray p = QFile::encodeName(path);
    FileState st;
    st.state = QString::fromLatin1(getx(p, "user.onedriver.state")).trimmed();
    if (!st.valid())
        return st;
    st.pinned = getx(p, "user.onedriver.pinned").trimmed() == "1";
    return st;
}

bool setPinnedXattr(const QString &path, bool pinned)
{
    const QByteArray v = pinned ? "1" : "0";
    return lsetxattr(QFile::encodeName(path).constData(), "user.onedriver.pinned", v.constData(), v.size(), 0) == 0;
}

bool setEvictXattr(const QString &path)
{
    return lsetxattr(QFile::encodeName(path).constData(), "user.onedriver.evict", "1", 1, 0) == 0;
}

} // namespace OneDriverDolphin
