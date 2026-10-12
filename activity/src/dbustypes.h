// D-Bus struct types of io.github.onedriver.Mount1, see docs/dbus-interface.md.
#pragma once

#include <QDBusArgument>
#include <QList>
#include <QMetaType>
#include <QString>

// one element of GetRecent: a(sxs)
struct RecentEntry {
    QString path;
    qint64 time = 0;
    QString action;
};

// one element of GetTransfers: a(sssttx)
struct TransferEntry {
    QString path;
    QString direction; // upload | download
    QString state;     // queued | active | retrying
    quint64 done = 0;
    quint64 total = 0;
    qint64 queued = 0;
};

Q_DECLARE_METATYPE(RecentEntry)
Q_DECLARE_METATYPE(TransferEntry)

QDBusArgument &operator<<(QDBusArgument &arg, const RecentEntry &e);
const QDBusArgument &operator>>(const QDBusArgument &arg, RecentEntry &e);
QDBusArgument &operator<<(QDBusArgument &arg, const TransferEntry &e);
const QDBusArgument &operator>>(const QDBusArgument &arg, TransferEntry &e);

void registerDBusTypes();
