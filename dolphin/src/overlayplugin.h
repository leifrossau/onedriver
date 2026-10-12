// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once

#include <KOverlayIconPlugin>

#include <QHash>
#include <QStringList>

/// Shows the onedriver file state (cloud / pinned / syncing / error) as
/// emblems in Dolphin. State comes from xattrs; D-Bus is only used to find
/// the mountpoints and to learn when states changed.
class OneDriverOverlayPlugin : public KOverlayIconPlugin
{
    Q_OBJECT

public:
    explicit OneDriverOverlayPlugin(QObject *parent = nullptr);

    QStringList getOverlays(const QUrl &item) override;

    /// called by the per-mount D-Bus watcher
    void handleFileStatesChanged(const QString &mountPoint, const QStringList &paths);

private Q_SLOTS:
    void onNameOwnerChanged(const QString &name, const QString &oldOwner, const QString &newOwner);

private:
    void addService(const QString &busName);
    void removeService(const QString &busName);

    QHash<QString, QObject *> m_watchers; // bus name -> MountWatcher
    QHash<QString, QString> m_mounts; // bus name -> mountpoint
    QStringList m_mountList;
};
