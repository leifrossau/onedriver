#include "models.h"

#include "helpers.h"

#include <QDateTime>
#include <QMimeDatabase>

static QString mimeIcon(const QString &fileName)
{
    static const QMimeDatabase db;
    const QString icon = db.mimeTypeForFile(fileName, QMimeDatabase::MatchExtension).iconName();
    return icon.isEmpty() ? QStringLiteral("application-octet-stream") : icon;
}

// ---- TransferModel ----

int TransferModel::rowCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : int(m_items.size());
}

QHash<int, QByteArray> TransferModel::roleNames() const
{
    return {{NameRole, "name"},
            {FolderRole, "folder"},
            {PathRole, "path"},
            {DirectionRole, "direction"},
            {StateRole, "xferState"},
            {StateTextRole, "stateText"},
            {FractionRole, "fraction"},
            {ProgressTextRole, "progressText"},
            {IconNameRole, "iconName"}};
}

QVariant TransferModel::data(const QModelIndex &index, int role) const
{
    if (!index.isValid() || index.row() < 0 || index.row() >= m_items.size()) {
        return {};
    }
    const TransferEntry &t = m_items.at(index.row());
    switch (role) {
    case NameRole:
        return helpers::splitPath(t.path).name;
    case FolderRole:
        return helpers::splitPath(t.path).dir;
    case PathRole:
        return t.path;
    case DirectionRole:
        return t.direction;
    case StateRole:
        return t.state;
    case StateTextRole:
        if (t.state == QLatin1String("queued")) {
            return QStringLiteral("Queued");
        }
        if (t.state == QLatin1String("retrying")) {
            return QStringLiteral("Retrying");
        }
        return t.direction == QLatin1String("upload") ? QStringLiteral("Uploading") : QStringLiteral("Downloading");
    case FractionRole:
        if (t.total == 0) {
            return -1.0;
        }
        return qBound(0.0, double(t.done) / double(t.total), 1.0);
    case ProgressTextRole:
        if (t.state == QLatin1String("queued")) {
            return t.total ? helpers::sizeText(t.total) : QString();
        }
        return helpers::progressText(t.done, t.total);
    case IconNameRole:
        return mimeIcon(t.path);
    }
    return {};
}

void TransferModel::setTransfers(const QList<TransferEntry> &transfers)
{
    bool sameRows = transfers.size() == m_items.size();
    for (qsizetype i = 0; sameRows && i < transfers.size(); ++i) {
        sameRows = transfers.at(i).path == m_items.at(i).path && transfers.at(i).direction == m_items.at(i).direction;
    }
    if (!sameRows) {
        beginResetModel();
        m_items = transfers;
        endResetModel();
        return;
    }
    for (int i = 0; i < transfers.size(); ++i) {
        const TransferEntry &n = transfers.at(i);
        const TransferEntry &o = m_items.at(i);
        if (n.state != o.state || n.done != o.done || n.total != o.total) {
            m_items[i] = n;
            emit dataChanged(index(i), index(i));
        }
    }
}

// ---- RecentModel ----

RecentModel::RecentModel(QObject *parent)
    : QAbstractListModel(parent), m_clock([] { return QDateTime::currentSecsSinceEpoch(); })
{
}

int RecentModel::rowCount(const QModelIndex &parent) const
{
    return parent.isValid() ? 0 : int(m_items.size());
}

QHash<int, QByteArray> RecentModel::roleNames() const
{
    return {{NameRole, "name"},
            {FolderRole, "folder"},
            {PathRole, "path"},
            {ActionRole, "syncAction"},
            {ActionTextRole, "actionText"},
            {ActionIconRole, "actionIcon"},
            {TimeTextRole, "timeText"},
            {IconNameRole, "iconName"}};
}

QVariant RecentModel::data(const QModelIndex &index, int role) const
{
    if (!index.isValid() || index.row() < 0 || index.row() >= m_items.size()) {
        return {};
    }
    const RecentEntry &e = m_items.at(index.row());
    switch (role) {
    case NameRole:
        return helpers::splitPath(e.path).name;
    case FolderRole:
        return helpers::splitPath(e.path).dir;
    case PathRole:
        return e.path;
    case ActionRole:
        return e.action;
    case ActionTextRole:
        if (e.action == QLatin1String("uploaded")) return QStringLiteral("Uploaded");
        if (e.action == QLatin1String("downloaded")) return QStringLiteral("Downloaded");
        if (e.action == QLatin1String("deleted")) return QStringLiteral("Deleted");
        if (e.action == QLatin1String("conflict")) return QStringLiteral("Conflict");
        if (e.action == QLatin1String("error")) return QStringLiteral("Error");
        return e.action.left(1).toUpper() + e.action.mid(1);
    case ActionIconRole:
        if (e.action == QLatin1String("uploaded")) return QStringLiteral("cloud-upload");
        if (e.action == QLatin1String("downloaded")) return QStringLiteral("cloud-download");
        if (e.action == QLatin1String("deleted")) return QStringLiteral("edit-delete");
        if (e.action == QLatin1String("conflict")) return QStringLiteral("dialog-warning");
        return QStringLiteral("dialog-error");
    case TimeTextRole:
        return helpers::relativeTime(e.time, m_clock());
    case IconNameRole:
        return mimeIcon(e.path);
    }
    return {};
}

void RecentModel::setEntries(const QList<RecentEntry> &entries)
{
    beginResetModel();
    m_items = entries;
    endResetModel();
}

void RecentModel::refreshTimes()
{
    if (!m_items.isEmpty()) {
        emit dataChanged(index(0), index(int(m_items.size()) - 1), {TimeTextRole});
    }
}
