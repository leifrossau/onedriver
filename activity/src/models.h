// List models for the "Syncing now" and "Recent" lists.
#pragma once

#include "dbustypes.h"

#include <QAbstractListModel>
#include <functional>

class TransferModel : public QAbstractListModel
{
    Q_OBJECT
public:
    enum Role {
        NameRole = Qt::UserRole + 1,
        FolderRole,
        PathRole,
        DirectionRole, // "upload" | "download"
        StateRole,     // "queued" | "active" | "retrying"
        StateTextRole,
        FractionRole, // 0..1, negative if the total is unknown
        ProgressTextRole,
        IconNameRole,
    };

    explicit TransferModel(QObject *parent = nullptr) : QAbstractListModel(parent) {}

    int rowCount(const QModelIndex &parent = {}) const override;
    QVariant data(const QModelIndex &index, int role) const override;
    QHash<int, QByteArray> roleNames() const override;

    // Replaces the content. If only progress/state changed (same files in the
    // same order) rows are updated in place so delegates survive.
    void setTransfers(const QList<TransferEntry> &transfers);

private:
    QList<TransferEntry> m_items;
};

class RecentModel : public QAbstractListModel
{
    Q_OBJECT
public:
    enum Role {
        NameRole = Qt::UserRole + 1,
        FolderRole,
        PathRole,
        ActionRole, // uploaded | downloaded | deleted | conflict | error
        ActionTextRole,
        ActionIconRole,
        TimeTextRole,
        IconNameRole,
    };

    explicit RecentModel(QObject *parent = nullptr);

    int rowCount(const QModelIndex &parent = {}) const override;
    QVariant data(const QModelIndex &index, int role) const override;
    QHash<int, QByteArray> roleNames() const override;

    void setEntries(const QList<RecentEntry> &entries);
    // Re-evaluates the relative times ("2 min ago") without new data.
    void refreshTimes();
    // For tests: replaces the clock (unix seconds).
    void setClock(std::function<qint64()> clock) { m_clock = std::move(clock); }

private:
    QList<RecentEntry> m_items;
    std::function<qint64()> m_clock;
};
