// SPDX-License-Identifier: GPL-3.0-or-later
#include "actionplugin.h"
#include "logic.h"

#include <KFileItemListProperties>
#include <KLocalizedString>
#include <KPluginFactory>

#include <QAction>
#include <QDBusConnection>
#include <QDBusConnectionInterface>
#include <QDBusMessage>
#include <QDBusReply>
#include <QDBusVariant>
#include <QDesktopServices>
#include <QFileInfo>
#include <QIcon>
#include <QMenu>
#include <QMessageBox>
#include <QUrl>

using namespace OneDriverDolphin;

K_PLUGIN_CLASS_WITH_JSON(OneDriverActionPlugin, "onedriveraction.json")

OneDriverActionPlugin::OneDriverActionPlugin(QObject *parent, const QVariantList &)
    : KAbstractFileItemActionPlugin(parent)
{
}

QList<QAction *> OneDriverActionPlugin::actions(const KFileItemListProperties &props, QWidget *parentWidget)
{
    // Only offer the menu if every selected item is a local file with onedriver
    // state xattrs, i.e. lives inside an onedriver mount. No D-Bus needed here.
    const QList<QUrl> urls = props.urlList();
    if (urls.isEmpty())
        return {};
    QStringList paths;
    bool allPinned = true;
    for (const QUrl &u : urls) {
        if (!u.isLocalFile())
            return {};
        const QString p = u.toLocalFile();
        const FileState st = readFileState(p);
        if (!st.valid())
            return {};
        allPinned = allPinned && st.pinned;
        paths << p;
    }

    auto *menu = new QMenu(i18nc("@title:menu", "OneDrive"), parentWidget);
    menu->setIcon(QIcon::fromTheme(QStringLiteral("folder-cloud")));

    auto *pin = menu->addAction(QIcon::fromTheme(QStringLiteral("emblem-success")), i18nc("@action:inmenu", "Always keep on this device"));
    pin->setCheckable(true);
    pin->setChecked(allPinned);
    connect(pin, &QAction::triggered, this, [paths](bool checked) {
        for (const QString &p : paths)
            setPinnedXattr(p, checked);
    });

    auto *free = menu->addAction(QIcon::fromTheme(QStringLiteral("download-later")), i18nc("@action:inmenu", "Free up space"));
    connect(free, &QAction::triggered, this, [paths]() {
        for (const QString &p : paths)
            setEvictXattr(p);
    });

    if (paths.size() == 1) {
        const QString path = paths.first();
        auto *web = menu->addAction(QIcon::fromTheme(QStringLiteral("internet-services")), i18nc("@action:inmenu", "View online"));
        connect(web, &QAction::triggered, this, [this, path, parentWidget]() {
            viewOnline(path, parentWidget);
        });
    }

    return {menu->menuAction()};
}

/// Finds the mount instance owning path (ListNames + MountPoint) and asks it
/// for the web URL. Blocking D-Bus calls, but only after an explicit click.
void OneDriverActionPlugin::viewOnline(const QString &path, QWidget *parent)
{
    QDBusConnection bus = QDBusConnection::sessionBus();
    const QString canon = QFileInfo(path).canonicalFilePath();
    const QString abs = path.isEmpty() ? path : QFileInfo(path).absoluteFilePath();
    const QStringList names = bus.interface()->registeredServiceNames();
    QString url;
    for (const QString &n : names) {
        if (!isMountBusName(n))
            continue;
        QDBusMessage get = QDBusMessage::createMethodCall(n,
                                                          QString::fromLatin1(kObjectPath),
                                                          QStringLiteral("org.freedesktop.DBus.Properties"),
                                                          QStringLiteral("Get"));
        get << QString::fromLatin1(kInterface) << QStringLiteral("MountPoint");
        QDBusReply<QDBusVariant> mp = bus.call(get, QDBus::Block, 3000);
        if (!mp.isValid())
            continue;
        QString mount = mp.value().variant().toString();
        const bool absIn = !mountForPath(abs, {mount}).isEmpty();
        if (!absIn && mountForPath(canon, {mount}).isEmpty())
            continue;
        const QString use = absIn ? abs : canon;
        QDBusMessage call = QDBusMessage::createMethodCall(n,
                                                           QString::fromLatin1(kObjectPath),
                                                           QString::fromLatin1(kInterface),
                                                           QStringLiteral("GetWebURL"));
        call << use;
        QDBusReply<QString> r = bus.call(call, QDBus::Block, 10000);
        if (r.isValid()) {
            url = r.value();
            break;
        }
    }
    if (url.isEmpty()) {
        QMessageBox::warning(parent,
                             i18nc("@title:window", "OneDrive"),
                             i18n("Could not get the online address of this item. Is onedriver running and online?"));
        return;
    }
    QDesktopServices::openUrl(QUrl(url));
}

#include "actionplugin.moc"
