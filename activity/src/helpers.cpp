#include "helpers.h"

#include <QDateTime>
#include <QLocale>

namespace helpers {

PathParts splitPath(const QString &path)
{
    QString p = path;
    while (p.endsWith(QLatin1Char('/')) && p.size() > 1) {
        p.chop(1);
    }
    const int i = p.lastIndexOf(QLatin1Char('/'));
    if (i < 0) {
        return {QString(), p};
    }
    return {p.left(i), p.mid(i + 1)};
}

QString relativeTime(qint64 unixTime, qint64 now)
{
    const qint64 diff = now - unixTime;
    if (diff < 45) { // also covers clock skew (negative)
        return QStringLiteral("just now");
    }
    if (diff < 3600) {
        return QStringLiteral("%1 min ago").arg(qMax<qint64>(1, (diff + 30) / 60));
    }
    const QDateTime then = QDateTime::fromSecsSinceEpoch(unixTime);
    const qint64 days = then.date().daysTo(QDateTime::fromSecsSinceEpoch(now).date());
    if (days == 0) {
        const qint64 h = diff / 3600;
        return h == 1 ? QStringLiteral("1 hour ago") : QStringLiteral("%1 hours ago").arg(h);
    }
    if (days == 1) {
        return QStringLiteral("yesterday");
    }
    if (days < 7) {
        return QStringLiteral("%1 days ago").arg(days);
    }
    return QLocale().toString(then.date(), QLocale::ShortFormat);
}

QString sizeText(quint64 bytes)
{
    return QLocale().formattedDataSize(qint64(bytes), 1, QLocale::DataSizeSIFormat);
}

QString progressText(quint64 done, quint64 total)
{
    if (total == 0) {
        return sizeText(done);
    }
    return QStringLiteral("%1 of %2").arg(sizeText(done), sizeText(total));
}

// OneDrive counts (and its website shows) the quota in units of 1024, so
// "1 TB" is 1 TiB. Use the same units, or 191.6 GB used shows up as 205.7 GB.
static QString quotaSizeText(quint64 bytes)
{
    return QLocale().formattedDataSize(qint64(bytes), 1, QLocale::DataSizeTraditionalFormat);
}

QString quotaText(quint64 used, quint64 total)
{
    if (total == 0) {
        return QString();
    }
    return QStringLiteral("Used %1 of %2").arg(quotaSizeText(used), quotaSizeText(total));
}

QString instanceLabel(const QString &account, const QString &mountPoint)
{
    QString dir = splitPath(mountPoint).name;
    if (dir.isEmpty()) {
        dir = mountPoint;
    }
    if (account.isEmpty()) {
        return dir;
    }
    return QStringLiteral("%1 (%2)").arg(account, dir);
}

} // namespace helpers
