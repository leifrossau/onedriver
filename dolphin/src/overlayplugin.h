// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once

#include <KOverlayIconPlugin>

#include <QHash>
#include <QStringList>

/// Shows the onedriver file state (cloud / pinned / syncing / error) as
/// emblems in Dolphin. State comes from xattrs; D-Bus is only used to find
/// the mountpoints and to learn when states changed.
// Dolphin loads overlay plugins with QPluginLoader and qobject_casts the
// root object to KOverlayIconPlugin, so the class itself must be the Qt
// plugin (K_PLUGIN_CLASS_WITH_JSON would make a factory the root object and
// Dolphin silently drops the plugin).
class OneDriverOverlayPlugin : public KOverlayIconPlugin
{
    Q_OBJECT
    Q_PLUGIN_METADATA(IID "org.kde.overlayicon.onedriver" FILE "onedriveroverlay.json")

public:
    explicit OneDriverOverlayPlugin(QObject *parent = nullptr);

    QStringList getOverlays(const QUrl &item) override;

    /// called by the per-mount D-Bus watcher
    void handleFileStatesChanged(const QString &mountPoint, const QStringList &paths);

private Q_SLOTS:
    void onNameOwnerChanged(const QString &name, const QString &oldOwner, const QString &newOwner);

private:
    void addService(const QString &busName);
    void addMount(const QString &busName, QString mountPoint);
    void removeService(const QString &busName);

    QHash<QString, QObject *> m_watchers; // bus name -> MountWatcher
    QHash<QString, QString> m_mounts; // bus name -> mountpoint
    QStringList m_mountList;
    // local paths asked about while no mount matched them (e.g. Dolphin was
    // started before onedriver); refreshed when a mount appears
    QList<QString> m_unmatched;
};
