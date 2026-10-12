#include "backend.h"

#include "helpers.h"

#include <QDBusConnection>
#include <QDBusError>
#include <QDBusMessage>
#include <QDBusPendingCallWatcher>
#include <QDBusPendingReply>
#include <QDesktopServices>
#include <QFileInfo>
#include <QProcess>
#include <QUrl>

namespace {
const QString kPrefix = QStringLiteral("io.github.onedriver.Mount.");
const QString kPath = QStringLiteral("/io/github/onedriver/Mount");
const QString kIface = QStringLiteral("io.github.onedriver.Mount1");
const QString kPropsIface = QStringLiteral("org.freedesktop.DBus.Properties");

QDBusMessage mountCall(const QString &service, const QString &method, const QVariantList &args = {})
{
    QDBusMessage m = QDBusMessage::createMethodCall(service, kPath, kIface, method);
    m.setArguments(args);
    return m;
}

// Runs fn(reply) on the GUI thread once the call has finished.
template<typename Reply, typename Fn>
void asyncCall(QObject *parent, const QDBusMessage &msg, Fn fn)
{
    auto *watcher = new QDBusPendingCallWatcher(QDBusConnection::sessionBus().asyncCall(msg), parent);
    QObject::connect(watcher, &QDBusPendingCallWatcher::finished, parent, [watcher, fn] {
        const Reply reply = *watcher;
        fn(reply);
        watcher->deleteLater();
    });
}

bool isMissingMethod(const QDBusError &e)
{
    return e.type() == QDBusError::UnknownMethod || e.type() == QDBusError::UnknownInterface;
}
} // namespace

Backend::Backend(QObject *parent) : QObject(parent)
{
    m_debounce.setSingleShot(true);
    m_debounce.setInterval(250);
    connect(&m_debounce, &QTimer::timeout, this, &Backend::refresh);
    m_tick.setInterval(30 * 1000);
    connect(&m_tick, &QTimer::timeout, &m_recent, &RecentModel::refreshTimes);

    QDBusConnection::sessionBus().connect(QStringLiteral("org.freedesktop.DBus"), QStringLiteral("/org/freedesktop/DBus"),
                                          QStringLiteral("org.freedesktop.DBus"), QStringLiteral("NameOwnerChanged"), this,
                                          SLOT(onNameOwnerChanged(QString, QString, QString)));
}

// ---- properties for QML ----

QString Backend::currentService() const
{
    return m_current >= 0 && m_current < m_instances.size() ? m_instances.at(m_current).service : QString();
}

QStringList Backend::instanceLabels() const
{
    QStringList l;
    for (const Instance &i : m_instances) {
        l << i.label;
    }
    return l;
}

void Backend::setCurrentIndex(int index)
{
    if (index == m_current || index < 0 || index >= m_instances.size()) {
        return;
    }
    subscribe(false);
    m_current = index;
    m_status.clear();
    m_transfers.setTransfers({});
    m_recent.setEntries({});
    emit instancesChanged();
    emit statusChanged();
    if (m_visible) {
        subscribe(true);
        refresh();
    }
}

QString Backend::account() const
{
    return m_status.value(QStringLiteral("Account")).toString();
}

bool Backend::isError() const
{
    return m_status.value(QStringLiteral("State")).toString() == QLatin1String("error");
}

QString Backend::stateText() const
{
    if (!connected()) {
        return QStringLiteral("OneDrive is not running");
    }
    const QString state = m_status.value(QStringLiteral("State")).toString();
    if (state == QLatin1String("paused") || paused()) {
        return QStringLiteral("Paused");
    }
    if (state == QLatin1String("offline")) {
        return QStringLiteral("Offline");
    }
    if (state == QLatin1String("error")) {
        const QString err = m_status.value(QStringLiteral("LastError")).toString();
        return err.isEmpty() ? QStringLiteral("Sync error") : err;
    }
    if (state == QLatin1String("syncing")) {
        qsizetype n = m_transfersSupported
            ? m_transfers.rowCount()
            : m_status.value(QStringLiteral("PendingUploads")).toUInt() + m_status.value(QStringLiteral("PendingDownloads")).toUInt();
        if (n == 0) {
            return QStringLiteral("Syncing");
        }
        return n == 1 ? QStringLiteral("Syncing 1 file") : QStringLiteral("Syncing %1 files").arg(n);
    }
    return QStringLiteral("Up to date");
}

QString Backend::stateIcon() const
{
    if (!connected()) {
        return QStringLiteral("state-warning");
    }
    const QString state = m_status.value(QStringLiteral("State")).toString();
    if (state == QLatin1String("paused") || paused()) {
        return QStringLiteral("state-pause");
    }
    if (state == QLatin1String("offline")) {
        return QStringLiteral("state-offline");
    }
    if (state == QLatin1String("error")) {
        return QStringLiteral("state-error");
    }
    if (state == QLatin1String("syncing")) {
        return QStringLiteral("state-sync");
    }
    return QStringLiteral("state-ok");
}

QString Backend::quotaText() const
{
    return helpers::quotaText(m_status.value(QStringLiteral("QuotaUsed")).toULongLong(),
                              m_status.value(QStringLiteral("QuotaTotal")).toULongLong());
}

double Backend::quotaFraction() const
{
    const double total = m_status.value(QStringLiteral("QuotaTotal")).toDouble();
    return total > 0 ? qBound(0.0, m_status.value(QStringLiteral("QuotaUsed")).toDouble() / total, 1.0) : 0.0;
}

// ---- visibility, discovery, subscriptions ----

void Backend::setVisible(bool visible)
{
    if (visible == m_visible) {
        return;
    }
    m_visible = visible;
    if (visible) {
        m_tick.start();
        discover();
    } else {
        m_tick.stop();
        m_debounce.stop();
        subscribe(false);
    }
}

void Backend::onNameOwnerChanged(const QString &name, const QString &, const QString &)
{
    if (m_visible && name.startsWith(kPrefix)) {
        discover();
    }
}

void Backend::discover()
{
    const QDBusMessage msg = QDBusMessage::createMethodCall(QStringLiteral("org.freedesktop.DBus"), QStringLiteral("/org/freedesktop/DBus"),
                                                            QStringLiteral("org.freedesktop.DBus"), QStringLiteral("ListNames"));
    asyncCall<QDBusPendingReply<QStringList>>(this, msg, [this](const QDBusPendingReply<QStringList> &reply) {
        if (reply.isError() || !m_visible) {
            return;
        }
        const QString previous = currentService();
        QStringList services;
        for (const QString &n : reply.value()) {
            if (n.startsWith(kPrefix)) {
                services << n;
            }
        }
        services.sort();

        QMap<QString, QString> oldLabels;
        for (const Instance &i : std::as_const(m_instances)) {
            oldLabels.insert(i.service, i.label);
        }
        subscribe(false);
        m_instances.clear();
        for (const QString &s : std::as_const(services)) {
            m_instances.append({s, oldLabels.value(s, s.mid(kPrefix.size()))});
        }
        m_current = m_instances.isEmpty() ? -1 : 0;
        for (int i = 0; i < m_instances.size(); ++i) {
            if (m_instances.at(i).service == previous) {
                m_current = i;
            }
        }
        if (currentService() != previous) {
            m_status.clear();
            m_transfers.setTransfers({});
            m_recent.setEntries({});
        }
        emit instancesChanged();
        emit statusChanged();
        for (const Instance &i : std::as_const(m_instances)) {
            if (i.service != currentService()) {
                fetchStatus(i.service); // only to learn the account name for the selector
            }
        }
        subscribe(true);
        refresh();
    });
}

void Backend::subscribe(bool on)
{
    auto bus = QDBusConnection::sessionBus();
    if (!on) {
        if (m_subscribed.isEmpty()) {
            return;
        }
        bus.disconnect(m_subscribed, kPath, kPropsIface, QStringLiteral("PropertiesChanged"), this, SLOT(scheduleRefresh()));
        bus.disconnect(m_subscribed, kPath, kIface, QStringLiteral("TransfersChanged"), this, SLOT(scheduleRefresh()));
        bus.disconnect(m_subscribed, kPath, kIface, QStringLiteral("Notify"), this, SLOT(scheduleRefresh()));
        m_subscribed.clear();
        return;
    }
    const QString service = currentService();
    if (service.isEmpty() || service == m_subscribed) {
        return;
    }
    subscribe(false);
    bus.connect(service, kPath, kPropsIface, QStringLiteral("PropertiesChanged"), this, SLOT(scheduleRefresh()));
    bus.connect(service, kPath, kIface, QStringLiteral("TransfersChanged"), this, SLOT(scheduleRefresh()));
    bus.connect(service, kPath, kIface, QStringLiteral("Notify"), this, SLOT(scheduleRefresh()));
    m_subscribed = service;
}

// ---- fetching ----

void Backend::scheduleRefresh()
{
    if (m_visible && !m_debounce.isActive()) {
        m_debounce.start();
    }
}

void Backend::refresh()
{
    const QString service = currentService();
    if (service.isEmpty()) {
        return;
    }
    fetchStatus(service);
    if (m_transfersSupported) {
        fetchTransfers(service);
    }
    fetchRecent(service);
}

void Backend::applyStatus(const QVariantMap &status)
{
    m_status = status;
    emit statusChanged();
}

void Backend::fetchStatus(const QString &service)
{
    asyncCall<QDBusPendingReply<QVariantMap>>(this, mountCall(service, QStringLiteral("GetStatus")),
                                              [this, service](const QDBusPendingReply<QVariantMap> &reply) {
        if (reply.isError()) {
            return;
        }
        const QVariantMap status = reply.value();
        for (int i = 0; i < m_instances.size(); ++i) {
            if (m_instances.at(i).service == service) {
                const QString label = helpers::instanceLabel(status.value(QStringLiteral("Account")).toString(),
                                                             status.value(QStringLiteral("MountPoint")).toString());
                if (m_instances[i].label != label) {
                    m_instances[i].label = label;
                    emit instancesChanged();
                }
            }
        }
        if (service == currentService()) {
            applyStatus(status);
        }
    });
}

void Backend::fetchTransfers(const QString &service)
{
    asyncCall<QDBusPendingReply<QList<TransferEntry>>>(this, mountCall(service, QStringLiteral("GetTransfers")),
                                                       [this, service](const QDBusPendingReply<QList<TransferEntry>> &reply) {
        if (service != currentService()) {
            return;
        }
        if (reply.isError()) {
            if (isMissingMethod(reply.error())) { // older daemon: show only "Recent"
                m_transfersSupported = false;
                emit transfersSupportedChanged();
                m_transfers.setTransfers({});
                emit statusChanged();
            }
            return;
        }
        m_transfers.setTransfers(reply.value());
        emit statusChanged(); // "Syncing N files"
    });
}

void Backend::fetchRecent(const QString &service)
{
    asyncCall<QDBusPendingReply<QList<RecentEntry>>>(this, mountCall(service, QStringLiteral("GetRecent"), {QVariant::fromValue(uint(30))}),
                                                     [this, service](const QDBusPendingReply<QList<RecentEntry>> &reply) {
        if (service == currentService() && !reply.isError()) {
            // Hidden files (.xdg-volume-info, .Trash-1000/...) are read by the
            // desktop itself and are noise here, unless something went wrong.
            QList<RecentEntry> entries;
            for (const RecentEntry &e : reply.value()) {
                const bool hidden = e.path.startsWith(QLatin1Char('.')) || e.path.contains(QLatin1String("/."));
                if (!hidden || e.action == QLatin1String("conflict") || e.action == QLatin1String("error")) {
                    entries.append(e);
                }
            }
            m_recent.setEntries(entries);
        }
    });
}

// ---- actions ----

void Backend::callVoid(const QString &method)
{
    const QString service = currentService();
    if (service.isEmpty()) {
        return;
    }
    asyncCall<QDBusPendingReply<>>(this, mountCall(service, method), [this](const QDBusPendingReply<> &) { scheduleRefresh(); });
}

void Backend::togglePause()
{
    callVoid(paused() ? QStringLiteral("Resume") : QStringLiteral("Pause"));
}

void Backend::syncNow()
{
    callVoid(QStringLiteral("SyncNow"));
}

QString Backend::absPath(const QString &relative) const
{
    const QString mp = m_status.value(QStringLiteral("MountPoint")).toString();
    return relative.isEmpty() ? mp : mp + QLatin1Char('/') + relative;
}

void Backend::openMountPoint()
{
    QDesktopServices::openUrl(QUrl::fromLocalFile(absPath(QString())));
}

void Backend::openSettings()
{
    QProcess::startDetached(QStringLiteral("onedriver-launcher"), {});
}

void Backend::openFile(const QString &path)
{
    const QString abs = absPath(path);
    // deleted files no longer exist, show their folder instead
    if (QFileInfo::exists(abs)) {
        QDesktopServices::openUrl(QUrl::fromLocalFile(abs));
    } else {
        openFolder(path);
    }
}

void Backend::openFolder(const QString &path)
{
    QString dir = helpers::splitPath(path).dir;
    // walk up if the folder is gone as well
    while (!dir.isEmpty() && !QFileInfo::exists(absPath(dir))) {
        dir = helpers::splitPath(dir).dir;
    }
    QDesktopServices::openUrl(QUrl::fromLocalFile(absPath(dir)));
}

void Backend::viewOnline(const QString &path)
{
    const QString service = currentService();
    if (service.isEmpty()) {
        return;
    }
    asyncCall<QDBusPendingReply<QString>>(this, mountCall(service, QStringLiteral("GetWebURL"), {absPath(path)}),
                                          [](const QDBusPendingReply<QString> &reply) {
        if (!reply.isError() && !reply.value().isEmpty()) {
            QDesktopServices::openUrl(QUrl(reply.value()));
        }
    });
}
