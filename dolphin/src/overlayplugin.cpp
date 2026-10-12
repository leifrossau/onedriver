// SPDX-License-Identifier: GPL-3.0-or-later
#include "overlayplugin.h"
#include "logic.h"

#include <KPluginFactory>

#include <QDBusConnection>
#include <QDBusConnectionInterface>
#include <QDBusMessage>
#include <QDBusPendingCallWatcher>
#include <QDBusPendingReply>
#include <QDBusVariant>
#include <QFileInfo>
#include <QUrl>

using namespace OneDriverDolphin;

K_PLUGIN_CLASS_WITH_JSON(OneDriverOverlayPlugin, "onedriveroverlay.json")

/// Receives FileStatesChanged of one mount instance.
class MountWatcher : public QObject
{
    Q_OBJECT
public:
    MountWatcher(OneDriverOverlayPlugin *plugin, const QString &busName, const QString &mountPoint)
        : QObject(plugin)
        , m_plugin(plugin)
        , m_mountPoint(mountPoint)
    {
        QDBusConnection::sessionBus().connect(busName,
                                              QString::fromLatin1(kObjectPath),
                                              QString::fromLatin1(kInterface),
                                              QStringLiteral("FileStatesChanged"),
                                              this,
                                              SLOT(fileStatesChanged(QStringList)));
    }

public Q_SLOTS:
    void fileStatesChanged(const QStringList &paths)
    {
        m_plugin->handleFileStatesChanged(m_mountPoint, paths);
    }

private:
    OneDriverOverlayPlugin *m_plugin;
    QString m_mountPoint;
};

OneDriverOverlayPlugin::OneDriverOverlayPlugin(QObject *parent)
    : KOverlayIconPlugin(parent)
{
    QDBusConnection bus = QDBusConnection::sessionBus();
    if (!bus.isConnected())
        return;

    bus.connect(QStringLiteral("org.freedesktop.DBus"),
                QStringLiteral("/org/freedesktop/DBus"),
                QStringLiteral("org.freedesktop.DBus"),
                QStringLiteral("NameOwnerChanged"),
                this,
                SLOT(onNameOwnerChanged(QString, QString, QString)));

    // asynchronous: never block the file manager at startup
    auto *w = new QDBusPendingCallWatcher(bus.interface()->asyncCall(QStringLiteral("ListNames")), this);
    connect(w, &QDBusPendingCallWatcher::finished, this, [this](QDBusPendingCallWatcher *self) {
        QDBusPendingReply<QStringList> reply = *self;
        self->deleteLater();
        const QStringList names = reply.value();
        for (const QString &n : names) {
            if (isMountBusName(n))
                addService(n);
        }
    });
}

void OneDriverOverlayPlugin::addService(const QString &busName)
{
    if (m_mounts.contains(busName))
        return;
    QDBusMessage msg = QDBusMessage::createMethodCall(busName,
                                                      QString::fromLatin1(kObjectPath),
                                                      QStringLiteral("org.freedesktop.DBus.Properties"),
                                                      QStringLiteral("Get"));
    msg << QString::fromLatin1(kInterface) << QStringLiteral("MountPoint");
    auto *w = new QDBusPendingCallWatcher(QDBusConnection::sessionBus().asyncCall(msg), this);
    connect(w, &QDBusPendingCallWatcher::finished, this, [this, busName](QDBusPendingCallWatcher *self) {
        QDBusPendingReply<QDBusVariant> reply = *self;
        self->deleteLater();
        if (reply.isError())
            return;
        QString mp = reply.value().variant().toString();
        while (mp.size() > 1 && mp.endsWith(QLatin1Char('/')))
            mp.chop(1);
        if (mp.isEmpty())
            return;
        // resolve symlinks once so it matches the canonical paths KIO hands us
        const QString canon = QFileInfo(mp).canonicalFilePath();
        const QString mount = canon.isEmpty() ? mp : canon;
        m_mounts.insert(busName, mount);
        m_mountList = m_mounts.values();
        m_watchers.insert(busName, new MountWatcher(this, busName, mount));
    });
}

void OneDriverOverlayPlugin::removeService(const QString &busName)
{
    delete m_watchers.take(busName);
    m_mounts.remove(busName);
    m_mountList = m_mounts.values();
}

void OneDriverOverlayPlugin::onNameOwnerChanged(const QString &name, const QString &oldOwner, const QString &newOwner)
{
    if (!isMountBusName(name))
        return;
    if (!oldOwner.isEmpty())
        removeService(name);
    if (!newOwner.isEmpty())
        addService(name);
}

QStringList OneDriverOverlayPlugin::getOverlays(const QUrl &item)
{
    if (m_mountList.isEmpty() || !item.isLocalFile())
        return {};
    const QString path = item.toLocalFile();
    const QString mount = mountForPath(path, m_mountList);
    if (mount.isEmpty() || path == mount)
        return {};
    // the xattr call is a single cheap FUSE request, no D-Bus per file
    const FileState st = readFileState(path);
    if (!st.valid())
        return {};
    return overlaysFor(st, QFileInfo(path).isDir());
}

void OneDriverOverlayPlugin::handleFileStatesChanged(const QString &mountPoint, const QStringList &paths)
{
    // The daemon may send parent folders instead of children when many items
    // changed; we emit for exactly what it names, Dolphin refreshes those items.
    for (const QString &rel : paths) {
        const QUrl url = QUrl::fromLocalFile(absolutePath(mountPoint, rel));
        Q_EMIT overlaysChanged(url, getOverlays(url));
    }
}

#include "overlayplugin.moc"
