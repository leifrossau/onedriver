// SPDX-License-Identifier: GPL-3.0-or-later
#include "overlayplugin.h"
#include "logic.h"


#include <QDBusConnection>
#include <QDBusConnectionInterface>
#include <QDBusMessage>
#include <QDBusPendingCallWatcher>
#include <QDBusPendingReply>
#include <QDBusReply>
#include <QDBusVariant>
#include <QFileInfo>
#include <QUrl>

using namespace OneDriverDolphin;

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

    // The first lookup is synchronous (with short timeouts): Dolphin asks for
    // the overlays of a folder right after loading the plugin, and an answer
    // given before the mounts are known would never be corrected. Later
    // changes (onedriver starting or stopping) are handled asynchronously.
    const QDBusReply<QStringList> names = bus.interface()->registeredServiceNames();
    if (!names.isValid())
        return;
    for (const QString &n : names.value()) {
        if (!isMountBusName(n))
            continue;
        QDBusMessage msg = QDBusMessage::createMethodCall(n,
                                                          QString::fromLatin1(kObjectPath),
                                                          QStringLiteral("org.freedesktop.DBus.Properties"),
                                                          QStringLiteral("Get"));
        msg << QString::fromLatin1(kInterface) << QStringLiteral("MountPoint");
        const QDBusReply<QDBusVariant> reply = bus.call(msg, QDBus::Block, 300);
        if (reply.isValid())
            addMount(n, reply.value().variant().toString());
    }
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
        addMount(busName, reply.value().variant().toString());
    });
}

void OneDriverOverlayPlugin::addMount(const QString &busName, QString mp)
{
    while (mp.size() > 1 && mp.endsWith(QLatin1Char('/')))
        mp.chop(1);
    if (mp.isEmpty() || m_mounts.contains(busName))
        return;
    // resolve symlinks once so it matches the canonical paths KIO hands us
    const QString canon = QFileInfo(mp).canonicalFilePath();
    const QString mount = canon.isEmpty() ? mp : canon;
    m_mounts.insert(busName, mount);
    m_mountList = m_mounts.values();
    m_watchers.insert(busName, new MountWatcher(this, busName, mount));

    // items Dolphin asked about before this mount was known
    QList<QString> rest;
    for (const QString &path : std::as_const(m_unmatched)) {
        if (mountForPath(path, {mount}).isEmpty()) {
            rest.append(path);
            continue;
        }
        const QUrl url = QUrl::fromLocalFile(path);
        Q_EMIT overlaysChanged(url, getOverlays(url));
    }
    m_unmatched = rest;
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
    if (!item.isLocalFile())
        return {};
    const QString path = item.toLocalFile();
    const QString mount = m_mountList.isEmpty() ? QString() : mountForPath(path, m_mountList);
    if (mount.isEmpty()) {
        // remember a bounded number of recent paths in case a mount appears
        m_unmatched.append(path);
        if (m_unmatched.size() > 2000)
            m_unmatched.remove(0, m_unmatched.size() - 2000);
        return {};
    }
    if (path == mount)
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
